package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeFile writes a pipeline config into a temp dir and returns its path.
func writeFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "snorgd.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFileMissingYieldsDefaults(t *testing.T) {
	f, err := LoadFile(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadFile on a missing file returned %v, want nil", err)
	}
	if got := f.Pipeline.QuietPeriod.Duration(); got != defaultQuietPeriod {
		t.Errorf("quiet period = %v, want %v", got, defaultQuietPeriod)
	}
	// The pre-pipeline behaviour: ingest everything, nothing downstream.
	if len(f.Ingest.Rules) != 0 {
		t.Errorf("rules = %v, want none", f.Ingest.Rules)
	}
	if f.Dispatch.Enabled || f.Letter.Enabled {
		t.Errorf("dispatch=%v letter=%v, want both disabled", f.Dispatch.Enabled, f.Letter.Enabled)
	}
}

func TestLoadFileParsesEveryStage(t *testing.T) {
	f, err := LoadFile(writeFile(t, `
pipeline:
  quiet_period: 90s
ingest:
  rules:
    - match: "/Supernote/AI/*.note"
      tags: [ai, question]
    - match: "/Supernote/Scratch/**"
      skip: true
dispatch:
  enabled: true
  query: "templated & tag ^ai$"
letter:
  enabled: true
  prompt: "Answer well."
  topics:
    brief: "Be terse."
    deep: "Be thorough."
  default_topic: brief
  dropbox_dir: "/Supernote/Letters"
  pdf_command: "pandoc -o {{out}} {{in}}"
`))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	if got := f.Pipeline.QuietPeriod.Duration(); got != 90*time.Second {
		t.Errorf("quiet period = %v, want 90s", got)
	}
	if len(f.Ingest.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(f.Ingest.Rules))
	}
	if got := f.Ingest.Rules[0].Tags; len(got) != 2 || got[0] != "ai" {
		t.Errorf("rule 0 tags = %v, want [ai question]", got)
	}
	if !f.Ingest.Rules[1].Skip {
		t.Error("rule 1 should be a skip rule")
	}
	if f.Dispatch.Query != "templated & tag ^ai$" {
		t.Errorf("dispatch query = %q, want the configured one", f.Dispatch.Query)
	}
	// A configured topic set replaces the defaults wholesale rather than merging
	// with them: the topics are the form's radio options, and a silently added
	// option would be a box on the template nobody asked for.
	if len(f.Letter.Topics) != 2 {
		t.Errorf("topics = %v, want only the configured two", f.Letter.Topics)
	}
	// Defaults fill in around what was written.
	if f.Letter.AnsweredTag != defaultAnsweredTag {
		t.Errorf("answered_tag = %q, want %q", f.Letter.AnsweredTag, defaultAnsweredTag)
	}
	if f.Letter.PDFName != defaultPDFName {
		t.Errorf("pdf_name = %q, want %q", f.Letter.PDFName, defaultPDFName)
	}
	if f.Letter.PDFTimeout.Duration() != defaultPDFTimeout {
		t.Errorf("pdf_timeout = %v, want %v", f.Letter.PDFTimeout.Duration(), defaultPDFTimeout)
	}
	if f.Letter.Prompt != "Answer well." {
		t.Errorf("prompt = %q, want the configured one", f.Letter.Prompt)
	}
}

func TestLoadFileEnabledLetterNeedsNoTopicsOfItsOwn(t *testing.T) {
	f, err := LoadFile(writeFile(t, "letter:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if len(f.Letter.Topics) != len(defaultTopics) {
		t.Errorf("topics = %v, want the built-in set", f.Letter.Topics)
	}
	if _, ok := f.Letter.Topics[f.Letter.DefaultTopic]; !ok {
		t.Errorf("default_topic %q is not in the default topic set", f.Letter.DefaultTopic)
	}
}

func TestTopicNamesAreSorted(t *testing.T) {
	// The order is the radio's layout order, so it must not come from map
	// iteration: regenerating a template from an unchanged config has to
	// reproduce the same image.
	l := Letter{Topics: map[string]string{"zeta": "z", "alpha": "a", "mid": "m"}}
	got := l.TopicNames()
	want := []string{"alpha", "mid", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TopicNames() = %v, want %v", got, want)
		}
	}
}

func TestLoadFileRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown key", "pipeline:\n  quiet_periode: 5s\n"},
		{"duration as a bare number", "pipeline:\n  quiet_period: 5\n"},
		{"invalid duration", "pipeline:\n  quiet_period: \"5 fortnights\"\n"},
		{"invalid glob", "ingest:\n  rules:\n    - match: \"/a/[unclosed\"\n"},
		{"analyze tag both included and excluded", "analyze:\n  enabled: true\n  rules:\n    - tags: [notes]\n      exclude: [notes]\n"},
		{"analyze rule with an empty tag", "analyze:\n  rules:\n    - tags: [\"  \"]\n"},
		{"misspelled key inside analyze", "analyze:\n  rules:\n    - tagz: [notes]\n"},
		{"default_topic naming no topic", "letter:\n  enabled: true\n  topics:\n    brief: \"Be terse.\"\n  default_topic: deep\n"},
		{"topic name the id grammar rejects", "letter:\n  enabled: true\n  topics:\n    \"two words\": \"x\"\n  default_topic: \"two words\"\n"},
		{"topic with an empty prompt", "letter:\n  enabled: true\n  topics:\n    brief: \"   \"\n  default_topic: brief\n"},
		{"pdf_command without dropbox_dir", "letter:\n  enabled: true\n  pdf_command: \"pandoc -o {{out}} {{in}}\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := LoadFile(writeFile(t, tc.body)); err == nil {
				t.Fatal("LoadFile accepted invalid config, want error")
			}
		})
	}
}

func TestFilePathPrefersEnvOverride(t *testing.T) {
	if got := FilePath(&Config{Archive: "/data/archive"}); got != "/data/archive/snorgd.yaml" {
		t.Errorf("FilePath = %q, want the archive default", got)
	}
	if got := FilePath(&Config{Archive: "/data/archive", ConfigFile: "/etc/snorgd.yaml"}); got != "/etc/snorgd.yaml" {
		t.Errorf("FilePath = %q, want the override", got)
	}
}

func TestLoadFileReadsAnalyzeRules(t *testing.T) {
	f, err := LoadFile(writeFile(t, "analyze:\n  enabled: true\n  rules:\n    - tags: [notes, fizyka]\n      exclude: [no-ai]\n    - exclude: [ai-answered]\n"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !f.Analyze.Enabled {
		t.Error("analyze.enabled = false, want true")
	}
	if len(f.Analyze.Rules) != 2 {
		t.Fatalf("rules = %v, want 2", f.Analyze.Rules)
	}
	if got := f.Analyze.Rules[0].Tags; len(got) != 2 || got[0] != "notes" || got[1] != "fizyka" {
		t.Errorf("rule 0 tags = %v, want [notes fizyka]", got)
	}
	if got := f.Analyze.Rules[0].Exclude; len(got) != 1 || got[0] != "no-ai" {
		t.Errorf("rule 0 exclude = %v, want [no-ai]", got)
	}
	// An exclude-only rule is legal: it is how "everything but these" is spelled.
	if len(f.Analyze.Rules[1].Tags) != 0 {
		t.Errorf("rule 1 tags = %v, want none", f.Analyze.Rules[1].Tags)
	}
}

func TestLoadFileDefaultsAnalyzeOff(t *testing.T) {
	// A config that never mentions analyze must not start spending model calls.
	f, err := LoadFile(writeFile(t, "pipeline:\n  quiet_period: 5s\n"))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if f.Analyze.Enabled {
		t.Error("analyze.enabled = true for a config that does not mention it")
	}
}
