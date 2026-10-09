package dropbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jdlugosz963/snorgd/internal/config"
)

// testClient returns a Client whose token and upload endpoints point at srv, with a
// pre-primed access token so no refresh round trip is needed.
func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClient(&config.Config{DropboxRefreshToken: "refresh", DropboxAppKey: "k", DropboxAppSecret: "s"})
	c.ep.token = srv.URL + "/token"
	c.ep.upload = srv.URL + "/upload"
	return c
}

func TestUploadPostsBytesWithOverwriteMode(t *testing.T) {
	var gotArg, gotAuth, gotType string
	var gotBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"tok-123","expires_in":14400}`))
		case "/upload":
			gotArg = r.Header.Get("Dropbox-API-Arg")
			gotAuth = r.Header.Get("Authorization")
			gotType = r.Header.Get("Content-Type")
			gotBody, _ = io.ReadAll(r.Body)
			w.Write([]byte(`{"name":"letters.pdf"}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	want := []byte("%PDF-1.4 pretend")
	if err := testClient(t, srv).Upload(context.Background(), "/Supernote/Letters/letters.pdf", want); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if string(gotBody) != string(want) {
		t.Errorf("uploaded body = %q, want %q", gotBody, want)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want the refreshed token", gotAuth)
	}
	if gotType != "application/octet-stream" {
		t.Errorf("Content-Type = %q", gotType)
	}

	var arg struct {
		Path       string `json:"path"`
		Mode       string `json:"mode"`
		Mute       bool   `json:"mute"`
		Autorename bool   `json:"autorename"`
	}
	if err := json.Unmarshal([]byte(gotArg), &arg); err != nil {
		t.Fatalf("Dropbox-API-Arg %q is not valid JSON: %v", gotArg, err)
	}
	if arg.Path != "/Supernote/Letters/letters.pdf" {
		t.Errorf("arg path = %q", arg.Path)
	}
	// Overwrite (not autorename) is what keeps the letters PDF a single growing file.
	if arg.Mode != "overwrite" || arg.Autorename || !arg.Mute {
		t.Errorf("arg = %+v, want overwrite/mute/no-autorename", arg)
	}
}

func TestUploadSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Write([]byte(`{"access_token":"tok","expires_in":14400}`))
			return
		}
		http.Error(w, "path/conflict", http.StatusConflict)
	}))
	defer srv.Close()

	err := testClient(t, srv).Upload(context.Background(), "/x/y.pdf", []byte("data"))
	if err == nil {
		t.Fatal("Upload returned nil on a 409, want an error")
	}
}

func TestAPIArgHeaderEscapesNonASCII(t *testing.T) {
	// HTTP headers are ASCII: a Polish folder name must survive as \uXXXX escapes,
	// or Go refuses to send the request at all.
	got, err := apiArgHeader(map[string]string{"path": "/Zeszyt/ćwiczenia.pdf"})
	if err != nil {
		t.Fatalf("apiArgHeader: %v", err)
	}
	for i := 0; i < len(got); i++ {
		if got[i] > 0x7f {
			t.Fatalf("apiArgHeader produced a non-ASCII byte at %d: %q", i, got)
		}
	}
	// It must still be JSON that decodes back to the original path.
	var back struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("escaped arg is not valid JSON: %v (%q)", err, got)
	}
	if back.Path != "/Zeszyt/ćwiczenia.pdf" {
		t.Errorf("round-tripped path = %q, want the original", back.Path)
	}
}
