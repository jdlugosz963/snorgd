// Package dropbox implements a watch.Watcher over a Dropbox folder: it does one full
// listing on first run (or resumes from a persisted cursor) and then long-polls for
// changes, downloading each changed *.note to a temp file and emitting a watch.Event.
package dropbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdlugosz963/snorgd/internal/config"
	"github.com/jdlugosz963/snorgd/internal/watch"
)

// Dropbox HTTP API endpoints (see https://www.dropbox.com/developers/documentation/http).
const (
	dbxTokenURL    = "https://api.dropbox.com/oauth2/token"
	dbxListURL     = "https://api.dropboxapi.com/2/files/list_folder"
	dbxContinueURL = "https://api.dropboxapi.com/2/files/list_folder/continue"
	dbxLongpollURL = "https://notify.dropboxapi.com/2/files/list_folder/longpoll"
	dbxDownloadURL = "https://content.dropboxapi.com/2/files/download"

	dbxPollTimeout = 30 // seconds; Dropbox allows 30..480
)

// Watcher watches a Dropbox folder for changed *.note files.
type Watcher struct {
	cfg   *config.Config
	httpc *http.Client // RPC + content requests
	pollc *http.Client // long-poll: outlives dbxPollTimeout

	accessToken string
	tokenExpiry time.Time

	cursorPath string
	seenRev    map[string]string // path_lower -> last rev, drops duplicate wakeups
}

// New returns a Watcher for the Dropbox folder configured in cfg.
func New(cfg *config.Config) *Watcher {
	return &Watcher{
		cfg:        cfg,
		httpc:      &http.Client{Timeout: 60 * time.Second},
		pollc:      &http.Client{Timeout: (dbxPollTimeout + 90) * time.Second},
		cursorPath: filepath.Join(cfg.StateDir, "dropbox.cursor"),
		seenRev:    map[string]string{},
	}
}

func (d *Watcher) Name() string { return "dropbox" }

// Watch resumes from the persisted cursor (or does a full initial listing), then
// long-polls indefinitely. Transient errors are logged and retried; it returns only
// when ctx is cancelled.
func (d *Watcher) Watch(ctx context.Context, out chan<- watch.Event) error {
	cursor := d.loadCursor()
	if cursor == "" {
		log.Printf("dropbox: initial listing of %q", d.folderArg())
		c, err := d.list(ctx, dbxListURL, listArg{Path: d.folderArg(), Recursive: true}, out)
		if err != nil {
			return fmt.Errorf("initial list: %w", err)
		}
		cursor = c
		d.saveCursor(cursor)
	} else {
		log.Printf("dropbox: resuming from saved cursor")
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed, backoff, err := d.longpoll(ctx, cursor)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("dropbox: longpoll error: %v (retrying in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return ctx.Err()
			}
			continue
		}
		if backoff > 0 {
			if !sleepCtx(ctx, time.Duration(backoff)*time.Second) {
				return ctx.Err()
			}
		}
		if !changed {
			continue
		}
		c, err := d.list(ctx, dbxContinueURL, continueArg{Cursor: cursor}, out)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("dropbox: fetch delta: %v (retrying in 5s)", err)
			if !sleepCtx(ctx, 5*time.Second) {
				return ctx.Err()
			}
			continue
		}
		cursor = c
		d.saveCursor(cursor)
	}
}

// folderArg maps the configured folder to the Dropbox path argument: the API wants
// "" for the root, otherwise a leading-slash path with no trailing slash.
func (d *Watcher) folderArg() string {
	f := strings.TrimRight(d.cfg.DropboxFolder, "/")
	if f == "" {
		return ""
	}
	if !strings.HasPrefix(f, "/") {
		f = "/" + f
	}
	return f
}

// list drives a list_folder (or list_folder/continue) request and pages through
// has_more, emitting an Event per changed *.note. It returns the final cursor.
func (d *Watcher) list(ctx context.Context, endpoint string, arg any, out chan<- watch.Event) (string, error) {
	for {
		body, err := d.rpc(ctx, endpoint, arg, true)
		if err != nil {
			return "", err
		}
		var res listResult
		if err := json.Unmarshal(body, &res); err != nil {
			return "", fmt.Errorf("decode list result: %w", err)
		}
		for _, e := range res.Entries {
			if err := d.handleEntry(ctx, e, out); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				log.Printf("dropbox: %s: %v (skipped)", e.PathDisplay, err)
			}
		}
		if !res.HasMore {
			return res.Cursor, nil
		}
		arg = continueArg{Cursor: res.Cursor}
		endpoint = dbxContinueURL
	}
}

// handleEntry downloads a changed *.note file entry and emits an Event. Non-file
// entries (folders, deletions) and non-.note files are ignored; deletions are logged
// since the daemon does not prune the archive (see README).
func (d *Watcher) handleEntry(ctx context.Context, e entry, out chan<- watch.Event) error {
	if e.Tag == "deleted" {
		if strings.HasSuffix(strings.ToLower(e.Name), ".note") {
			log.Printf("dropbox: %s deleted upstream — not removed from archive", e.PathDisplay)
		}
		return nil
	}
	if e.Tag != "file" || !strings.HasSuffix(strings.ToLower(e.Name), ".note") {
		return nil
	}
	if prev, ok := d.seenRev[e.PathLower]; ok && prev == e.Rev {
		return nil // duplicate wakeup for an unchanged revision
	}

	local, err := d.download(ctx, e.PathLower, e.Name)
	if err != nil {
		return err
	}
	d.seenRev[e.PathLower] = e.Rev
	select {
	case out <- watch.Event{Watcher: d.Name(), Source: e.PathDisplay, LocalPath: local}:
		return nil
	case <-ctx.Done():
		os.RemoveAll(filepath.Dir(local))
		return ctx.Err()
	}
}

// download fetches a file's bytes into a fresh temp directory, keeping the note's
// original filename so snorg names the note after the input file (not a temp name).
// It returns the path to the downloaded file; the caller removes its parent dir.
func (d *Watcher) download(ctx context.Context, pathLower, name string) (string, error) {
	tok, err := d.token(ctx)
	if err != nil {
		return "", err
	}
	apiArg, _ := json.Marshal(map[string]string{"path": pathLower})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dbxDownloadURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Dropbox-API-Arg", string(apiArg))

	resp, err := d.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("download %s: %s: %s", pathLower, resp.Status, strings.TrimSpace(string(b)))
	}

	dir, err := os.MkdirTemp("", "snorgd-")
	if err != nil {
		return "", err
	}
	// path.Base on the original name defends against a stray directory separator.
	local := filepath.Join(dir, path.Base(name))
	f, err := os.Create(local)
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.RemoveAll(dir)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return local, nil
}

// longpoll blocks up to dbxPollTimeout seconds for changes on cursor. It returns
// whether changes are pending and any server-requested backoff. The longpoll
// endpoint takes no auth (the cursor is the credential).
func (d *Watcher) longpoll(ctx context.Context, cursor string) (changed bool, backoff int, err error) {
	arg := map[string]any{"cursor": cursor, "timeout": dbxPollTimeout}
	body, err := json.Marshal(arg)
	if err != nil {
		return false, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dbxLongpollURL, bytes.NewReader(body))
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.pollc.Do(req)
	if err != nil {
		return false, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return false, 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var res struct {
		Changes bool `json:"changes"`
		Backoff int  `json:"backoff"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return false, 0, fmt.Errorf("decode longpoll: %w", err)
	}
	return res.Changes, res.Backoff, nil
}

// rpc POSTs a JSON arg to a Dropbox RPC endpoint and returns the response body,
// refreshing the access token once on a 401.
func (d *Watcher) rpc(ctx context.Context, endpoint string, arg any, retry bool) ([]byte, error) {
	tok, err := d.token(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(arg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized && retry {
		d.tokenExpiry = time.Time{} // force a refresh
		return d.rpc(ctx, endpoint, arg, false)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// token returns a valid short-lived access token, exchanging the refresh token when
// the cached one is missing or within a minute of expiry.
func (d *Watcher) token(ctx context.Context) (string, error) {
	if d.accessToken != "" && time.Until(d.tokenExpiry) > time.Minute {
		return d.accessToken, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {d.cfg.DropboxRefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dbxTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(d.cfg.DropboxAppKey, d.cfg.DropboxAppSecret)

	resp, err := d.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token refresh: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var res struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	d.accessToken = res.AccessToken
	d.tokenExpiry = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)
	return d.accessToken, nil
}

// loadCursor returns the persisted cursor for the configured folder, or "" when
// there is none (or it belongs to a different folder, forcing a fresh listing).
func (d *Watcher) loadCursor() string {
	raw, err := os.ReadFile(d.cursorPath)
	if err != nil {
		return ""
	}
	var s cursorState
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	if s.Folder != d.folderArg() {
		return ""
	}
	return s.Cursor
}

// saveCursor persists the cursor (best-effort; a failure only costs reprocessing on
// the next restart).
func (d *Watcher) saveCursor(cursor string) {
	if err := os.MkdirAll(filepath.Dir(d.cursorPath), 0o700); err != nil {
		log.Printf("dropbox: save cursor: %v", err)
		return
	}
	raw, _ := json.Marshal(cursorState{Folder: d.folderArg(), Cursor: cursor})
	if err := os.WriteFile(d.cursorPath, raw, 0o600); err != nil {
		log.Printf("dropbox: save cursor: %v", err)
	}
}

// sleepCtx sleeps for d unless ctx is cancelled first; it reports whether the full
// duration elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type cursorState struct {
	Folder string `json:"folder"`
	Cursor string `json:"cursor"`
}

type listArg struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

type continueArg struct {
	Cursor string `json:"cursor"`
}

type listResult struct {
	Entries []entry `json:"entries"`
	Cursor  string  `json:"cursor"`
	HasMore bool    `json:"has_more"`
}

type entry struct {
	Tag         string `json:".tag"` // "file" | "folder" | "deleted"
	Name        string `json:"name"`
	PathLower   string `json:"path_lower"`
	PathDisplay string `json:"path_display"`
	Rev         string `json:"rev"`
}
