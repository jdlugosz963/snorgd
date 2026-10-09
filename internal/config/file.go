package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jdlugosz963/snorgd/internal/analyze"
	"github.com/jdlugosz963/snorgd/internal/rules"
)

// File is the pipeline configuration: the parts that describe *how* notes are
// processed, as opposed to the environment's *where* and *with whose credentials*.
// It lives in a YAML file because rules are structured and env vars are not; secrets
// stay in the environment and never appear here, so this file is safe to commit
// alongside the notes it configures.
type File struct {
	Pipeline Pipeline `yaml:"pipeline"`
	Ingest   Ingest   `yaml:"ingest"`
	Analyze  Analyze  `yaml:"analyze"`
	Dispatch Dispatch `yaml:"dispatch"`
	Letter   Letter   `yaml:"letter"`
}

// Pipeline tunes the batching loop.
type Pipeline struct {
	// QuietPeriod is how long the queue must stay empty before the dispatch phase
	// fires. It absorbs the burst a Supernote sync produces, so one sync of five
	// notes costs one dispatch pass and one commit rather than five.
	QuietPeriod Duration `yaml:"quiet_period"`
}

// Ingest is the INGEST stage: which notes enter the archive, and how they are tagged.
type Ingest struct {
	Rules rules.Set `yaml:"rules"`
}

// Analyze is the ANALYZE stage: pages selected by their snorg tags are transcribed
// by the vision model. The prompts are not here — they live in the archive's own
// config.yaml under `analysis:`, next to the provider credentials snorg already reads
// from there, so this section only answers "which pages".
type Analyze struct {
	Enabled bool `yaml:"enabled"`
	// Rules select the pages by tag. They are the tag-side counterpart of the ingest
	// rules: those match a path and write note tags, these match the tags a page
	// ended up with — its note's plus its own — and admit it. No rules at all means
	// every page, the same convention the ingest rules use.
	//
	// There is deliberately no force flag and no `unanalyzed` narrowing: snorg skips
	// a page whose ink is unchanged since its last analysis, so re-running over the
	// whole selection every batch is free and a page rewritten on the device is
	// picked up without anyone asking.
	Rules analyze.Set `yaml:"rules"`
}

// Dispatch is the DISPATCH stage: templated pages are read back as snif forms and
// routed to the handler registered for their template.
type Dispatch struct {
	Enabled bool `yaml:"enabled"`
	// Query selects the pages to read, in snorg's query language. The default —
	// every page drawn on a configured template — is what the dispatcher wants in
	// nearly every case; narrowing it is a way to keep a busy archive cheap, since
	// each selected page costs a rasterization.
	Query string `yaml:"query"`
}

// Letter is the "letter to AI" handler: a form carrying a question, a length and a
// topic, answered by the model into the page's own answer region.
type Letter struct {
	Enabled bool `yaml:"enabled"`
	// Prompt is the base system instruction. The selected topic's preset and a
	// length instruction derived from the slider are appended to it.
	Prompt string `yaml:"prompt"`
	// Topics are the presets the form's radio offers, keyed by option id. The keys
	// become the radio's options, so the template and the prompts cannot drift.
	Topics map[string]string `yaml:"topics"`
	// DefaultTopic is used when no option is ticked — an unticked radio is not an
	// error in snif, it is simply unset.
	DefaultTopic string `yaml:"default_topic"`
	// AnsweredTag is the snorg tag written on a page once it has been answered. It
	// is a marker for a human browsing the archive; what stops an answer being
	// recomputed is the form state, not this tag.
	AnsweredTag string `yaml:"answered_tag"`
	// DropboxDir is where the assembled PDF is uploaded, and so where it appears on
	// the device.
	DropboxDir string `yaml:"dropbox_dir"`
	// PDFName is the single accumulating document's filename. Every run rewrites it
	// in place with the newest answer first, so the device sees one file growing
	// rather than a pile of one-answer files.
	PDFName string `yaml:"pdf_name"`
	// PDFCommand typesets the assembled markdown. It runs in a temp directory
	// holding letters.md and letters.tex, with {{in}} and {{out}} substituted, so
	// the whole look of the document is a config string plus a LaTeX template.
	// Empty disables the PDF entirely: the answer is in the archive either way.
	PDFCommand string `yaml:"pdf_command"`
	// PDFTimeout bounds one typesetting run, so a pathological document cannot
	// wedge the daemon.
	PDFTimeout Duration `yaml:"pdf_timeout"`
}

// Duration is a time.Duration that reads Go duration strings ("5s", "2m") from YAML.
// yaml.v3 has no native duration support and would otherwise demand a nanosecond
// integer, which is unreadable in a config file.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

const (
	defaultQuietPeriod = 5 * time.Second
	defaultPDFTimeout  = 2 * time.Minute
	defaultDispatchQ   = "templated"
	defaultAnsweredTag = "ai-answered"
	defaultPDFName     = "letters.pdf"
	defaultTopic       = "general"
	defaultPrompt      = "Answer the following handwritten question directly and completely, in the language it is written in."
)

// defaultTopics is the preset set a letter form offers when the config names none.
// The keys are the radio's options, so this is also the default template's layout.
var defaultTopics = map[string]string{
	"general": "Answer plainly, without preamble.",
	"study":   "Explain as a tutor would: define the terms, work through the reasoning step by step, and close with a short summary.",
	"code":    "Answer as an experienced programmer. Give complete, runnable code in a fenced block and explain the parts that are not obvious.",
	"letter":  "Write a letter in the register the question asks for, ready to send as it stands.",
}

// TopicNames returns the topic keys in a stable order, which is the order the form's
// radio lays its options out. Sorted rather than map order so regenerating a
// template from an unchanged config reproduces the same image, byte for byte.
func (l Letter) TopicNames() []string {
	names := make([]string, 0, len(l.Topics))
	for name := range l.Topics {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// FilePath resolves the pipeline config's location: SNORGD_CONFIG when set,
// otherwise snorgd.yaml inside the archive. Defaulting into the archive means the
// rules travel with the notes they describe and are editable from any clone.
func FilePath(cfg *Config) string {
	if cfg.ConfigFile != "" {
		return cfg.ConfigFile
	}
	return filepath.Join(cfg.Archive, "snorgd.yaml")
}

// LoadFile reads the pipeline configuration from path, applies defaults and
// validates it.
//
// A missing file is not an error: it yields defaults that reproduce the daemon's
// pre-pipeline behaviour (ingest everything untagged, no dispatch), so a fresh
// archive boots without one. Anything else — unreadable, malformed, unknown keys, a
// bad glob — fails here at startup rather than on the first note.
func LoadFile(path string) (*File, error) {
	f := &File{}
	raw, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		f.applyDefaults()
		return f, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	// Reject unknown keys: a misspelled "quiet_periode" that silently kept the
	// default would be near-impossible to notice in a daemon.
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	f.applyDefaults()
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

func (f *File) applyDefaults() {
	if f.Pipeline.QuietPeriod <= 0 {
		f.Pipeline.QuietPeriod = Duration(defaultQuietPeriod)
	}
	if strings.TrimSpace(f.Dispatch.Query) == "" {
		f.Dispatch.Query = defaultDispatchQ
	}
	if strings.TrimSpace(f.Letter.Prompt) == "" {
		f.Letter.Prompt = defaultPrompt
	}
	if len(f.Letter.Topics) == 0 {
		f.Letter.Topics = defaultTopics
	}
	if strings.TrimSpace(f.Letter.DefaultTopic) == "" {
		f.Letter.DefaultTopic = defaultTopic
	}
	if strings.TrimSpace(f.Letter.AnsweredTag) == "" {
		f.Letter.AnsweredTag = defaultAnsweredTag
	}
	if strings.TrimSpace(f.Letter.PDFName) == "" {
		f.Letter.PDFName = defaultPDFName
	}
	if f.Letter.PDFTimeout <= 0 {
		f.Letter.PDFTimeout = Duration(defaultPDFTimeout)
	}
}

// validate checks what can be checked without touching the archive. The dispatch
// query needs a snorg client to parse, so it is validated by the phase constructor —
// still at startup, just a few lines later.
func (f *File) validate() error {
	if err := f.Ingest.Rules.Validate(); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	if err := f.Analyze.Rules.Validate(); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	if !f.Letter.Enabled {
		return nil
	}
	// A topic key becomes a snif radio option id, which the id grammar restricts to
	// [A-Za-z0-9][A-Za-z0-9_.-]*. Catching it here names the offending key; letting
	// it through would fail later as an opaque template-generation error.
	for _, name := range f.Letter.TopicNames() {
		if !validTopicName(name) {
			return fmt.Errorf("letter: topic %q: a topic name must start with a letter or digit and hold only letters, digits, _ . or -", name)
		}
		if strings.TrimSpace(f.Letter.Topics[name]) == "" {
			return fmt.Errorf("letter: topic %q has an empty prompt", name)
		}
	}
	if _, ok := f.Letter.Topics[f.Letter.DefaultTopic]; !ok {
		return fmt.Errorf("letter: default_topic %q is not one of the topics (%s)",
			f.Letter.DefaultTopic, strings.Join(f.Letter.TopicNames(), ", "))
	}
	// The PDF is optional; its destination is only required once it is switched on.
	if strings.TrimSpace(f.Letter.PDFCommand) != "" && strings.TrimSpace(f.Letter.DropboxDir) == "" {
		return fmt.Errorf("letter: dropbox_dir is required when pdf_command is set")
	}
	return nil
}

func validTopicName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case i > 0 && (r == '_' || r == '.' || r == '-'):
		default:
			return false
		}
	}
	return true
}
