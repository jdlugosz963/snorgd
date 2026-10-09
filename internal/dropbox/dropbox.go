// Package dropbox implements a watch.Watcher over a Dropbox folder: it does one full
// listing on first run (or resumes from a persisted cursor) and then long-polls for
// changes, downloading each changed *.note to a temp file and emitting a watch.Event.
// The authenticated transport it rides on lives in Client, which the letter handler
// also uses to upload.
package dropbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jdlugosz963/snorgd/internal/config"
	"github.com/jdlugosz963/snorgd/internal/watch"
)

// Watcher watches a Dropbox folder for changed *.note files.
type Watcher struct {
	*Client
	pollc *http.Client // long-poll: outlives dbxPollTimeout

	cursorPath string
	seenRev    map[string]string // path_lower -> last rev, drops duplicate wakeups
}

// New returns a Watcher for the Dropbox folder configured in cfg, sharing c's token
// so the watcher and any uploader refresh credentials once between them.
func New(cfg *config.Config, c *Client) *Watcher {
	return &Watcher{
		Client:     c,
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
		c, err := d.list(ctx, d.ep.list, listArg{Path: d.folderArg(), Recursive: true}, out)
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
		c, err := d.list(ctx, d.ep.next, continueArg{Cursor: cursor}, out)
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
		endpoint = d.ep.next
	}
}

// handleEntry downloads a changed *.note file entry and emits an Event. Non-file
// entries (folders, deletions) and non-.note files are ignored; deletions are logged
// since the daemon does not prune the archive (see README).
//
// The .note filter is also what keeps the letter handler from feeding itself: the
// PDF it uploads lands back in this listing when letter.dropbox_dir sits inside the
// watched folder, and is dropped here. Relax this filter only with that in mind.
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

// longpoll blocks up to dbxPollTimeout seconds for changes on cursor. It returns
// whether changes are pending and any server-requested backoff. The longpoll
// endpoint takes no auth (the cursor is the credential).
func (d *Watcher) longpoll(ctx context.Context, cursor string) (changed bool, backoff int, err error) {
	arg := map[string]any{"cursor": cursor, "timeout": dbxPollTimeout}
	body, err := json.Marshal(arg)
	if err != nil {
		return false, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.ep.longpoll, bytes.NewReader(body))
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
