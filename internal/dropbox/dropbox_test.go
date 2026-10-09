package dropbox

import (
	"path/filepath"
	"testing"

	"github.com/jdlugosz963/snorgd/internal/config"
)

func TestCursorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{DropboxFolder: "/Supernote", StateDir: dir}
	d := New(cfg, NewClient(cfg))
	d.cursorPath = filepath.Join(dir, "dropbox.cursor")

	if got := d.loadCursor(); got != "" {
		t.Fatalf("fresh state: loadCursor = %q, want empty", got)
	}
	d.saveCursor("CURSOR-abc")
	if got := d.loadCursor(); got != "CURSOR-abc" {
		t.Fatalf("loadCursor = %q, want CURSOR-abc", got)
	}

	// A cursor saved for a different folder must be ignored (forces a fresh listing).
	otherCfg := &config.Config{DropboxFolder: "/Other", StateDir: dir}
	other := New(otherCfg, NewClient(otherCfg))
	other.cursorPath = d.cursorPath
	if got := other.loadCursor(); got != "" {
		t.Fatalf("cross-folder loadCursor = %q, want empty", got)
	}
}

func TestFolderArg(t *testing.T) {
	cases := map[string]string{
		"/Supernote":  "/Supernote",
		"Supernote":   "/Supernote",
		"/Supernote/": "/Supernote",
		"":            "",
		"/":           "",
	}
	for in, want := range cases {
		cfg := &config.Config{DropboxFolder: in}
		d := New(cfg, NewClient(cfg))
		if got := d.folderArg(); got != want {
			t.Errorf("folderArg(%q) = %q, want %q", in, got, want)
		}
	}
}
