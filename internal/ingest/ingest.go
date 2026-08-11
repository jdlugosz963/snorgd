// Package ingest wraps a snorg.Client and registers one downloaded .note at a time
// into the archive, honouring the archive's own config.yaml (ingest.svg toggles).
package ingest

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jdlugosz963/snorg/pkg/snorg"
)

// Ingestor registers a single .note into the archive at a time.
type Ingestor struct{ client *snorg.Client }

// New opens the archive at archive, applying its config.yaml if present.
func New(archive string) (*Ingestor, error) {
	var cfg *snorg.Config
	cfgPath := filepath.Join(archive, "config.yaml")
	if st, err := os.Stat(cfgPath); err == nil && !st.IsDir() {
		if cfg, err = snorg.LoadConfig([]string{cfgPath}); err != nil {
			return nil, fmt.Errorf("load %s: %w", cfgPath, err)
		}
	}
	client, err := snorg.Open(archive, cfg)
	if err != nil {
		return nil, err
	}
	return &Ingestor{client: client}, nil
}

// Ingest registers a single .note file into the archive. Re-ingesting a known note
// is an incremental reconcile handled by snorg, so a resaved note just updates its
// <FILE_ID>/ directory and preserves prior analysis.
func (i *Ingestor) Ingest(localPath string) (*snorg.Note, error) {
	res, err := i.client.Ingest([]string{localPath}, 1)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("no ingest result for %s", localPath)
	}
	if res[0].Err != nil {
		return nil, res[0].Err
	}
	return res[0].Note, nil
}
