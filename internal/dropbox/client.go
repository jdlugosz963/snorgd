package dropbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jdlugosz963/snorgd/internal/config"
)

// Dropbox HTTP API endpoints (see https://www.dropbox.com/developers/documentation/http).
const (
	dbxTokenURL    = "https://api.dropbox.com/oauth2/token"
	dbxListURL     = "https://api.dropboxapi.com/2/files/list_folder"
	dbxContinueURL = "https://api.dropboxapi.com/2/files/list_folder/continue"
	dbxLongpollURL = "https://notify.dropboxapi.com/2/files/list_folder/longpoll"
	dbxDownloadURL = "https://content.dropboxapi.com/2/files/download"
	dbxUploadURL   = "https://content.dropboxapi.com/2/files/upload"

	dbxPollTimeout = 30 // seconds; Dropbox allows 30..480
)

// endpoints are the API URLs, kept as fields rather than referenced as constants so
// a test can point them at an httptest server.
type endpoints struct {
	token    string
	list     string
	next     string
	longpoll string
	download string
	upload   string
}

func defaultEndpoints() endpoints {
	return endpoints{
		token:    dbxTokenURL,
		list:     dbxListURL,
		next:     dbxContinueURL,
		longpoll: dbxLongpollURL,
		download: dbxDownloadURL,
		upload:   dbxUploadURL,
	}
}

// Client is the authenticated Dropbox transport: OAuth token refresh plus the RPC
// and content calls built on it. It is shared by the Watcher (which reads) and the
// letter pipeline (which uploads), so both ride the same refreshed token rather than
// each holding their own credentials.
type Client struct {
	cfg   *config.Config
	httpc *http.Client
	ep    endpoints

	accessToken string
	tokenExpiry time.Time
}

// NewClient returns a Client for the Dropbox app configured in cfg.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		cfg:   cfg,
		httpc: &http.Client{Timeout: 60 * time.Second},
		ep:    defaultEndpoints(),
	}
}

// Upload writes data to dropboxPath, replacing whatever is there. Parent folders are
// created by Dropbox as needed.
//
// Overwrite (rather than autorename) is what makes the letters PDF a single growing
// document instead of an ever-multiplying pile of files, and mute suppresses the
// device notification each rewrite would otherwise raise.
func (c *Client) Upload(ctx context.Context, dropboxPath string, data []byte) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	apiArg, err := apiArgHeader(map[string]any{
		"path":       dropboxPath,
		"mode":       "overwrite",
		"mute":       true,
		"autorename": false,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.upload, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Dropbox-API-Arg", apiArg)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upload %s: %s: %s", dropboxPath, resp.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

// download fetches a file's bytes into a fresh temp directory, keeping the note's
// original filename so snorg names the note after the input file (not a temp name).
// It returns the path to the downloaded file; the caller removes its parent dir.
func (c *Client) download(ctx context.Context, pathLower, name string) (string, error) {
	tok, err := c.token(ctx)
	if err != nil {
		return "", err
	}
	apiArg, err := apiArgHeader(map[string]string{"path": pathLower})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.download, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Dropbox-API-Arg", apiArg)

	resp, err := c.httpc.Do(req)
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

// rpc POSTs a JSON arg to a Dropbox RPC endpoint and returns the response body,
// refreshing the access token once on a 401.
func (c *Client) rpc(ctx context.Context, endpoint string, arg any, retry bool) ([]byte, error) {
	tok, err := c.token(ctx)
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

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized && retry {
		c.tokenExpiry = time.Time{} // force a refresh
		return c.rpc(ctx, endpoint, arg, false)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s: %s", endpoint, resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, nil
}

// token returns a valid short-lived access token, exchanging the refresh token when
// the cached one is missing or within a minute of expiry.
func (c *Client) token(ctx context.Context) (string, error) {
	if c.accessToken != "" && time.Until(c.tokenExpiry) > time.Minute {
		return c.accessToken, nil
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {c.cfg.DropboxRefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(c.cfg.DropboxAppKey, c.cfg.DropboxAppSecret)

	resp, err := c.httpc.Do(req)
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
	c.accessToken = res.AccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

// apiArgHeader JSON-encodes v for the Dropbox-API-Arg header, escaping every
// non-ASCII rune as \uXXXX.
//
// HTTP header values are ASCII, and Dropbox documents this escaping for exactly that
// reason: a path containing (say) Polish diacritics would otherwise produce a
// request the server rejects — or that Go refuses to write at all.
func apiArgHeader(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range string(raw) {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		// utf16.Encode emits a surrogate pair for runes beyond the BMP, which is
		// what \u escaping requires.
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, "\\u%04x", u)
		}
	}
	return b.String(), nil
}
