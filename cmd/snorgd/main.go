// Command snorgd is a daemon that watches Dropbox for .note changes in real time and
// drives them through a configured pipeline: ingest into a local snorg archive (a
// clone of the notes repo) with rule-assigned tags, read every page drawn on a
// registered snif template and hand the ones that changed to the handler that owns
// them, then commit and push the whole batch as one change.
//
// Dropbox sits behind the watch.Watcher abstraction, so other note sources can be
// added without touching the daemon core, and a page's meaning lives in a
// dispatch.Handler, so a new kind of form is a new handler rather than a new stage.
//
// It also draws the templates it reads:
//
//	snorgd gen-templates            write each registered template's PNG and YAML here
//	snorgd gen-templates -print-tex show the LaTeX template the letters PDF uses
//
// Those files are written into the working directory and never into the archive:
// everything snorgd does to an archive goes through snorg's API, so placing a
// template in one and including it from its config.yaml stays a deliberate act.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"os/signal"

	"github.com/jdlugosz963/snif/pkg/snif"
	"github.com/jdlugosz963/snorg/pkg/snorg"

	"github.com/jdlugosz963/snorgd/internal/analyze"
	"github.com/jdlugosz963/snorgd/internal/config"
	"github.com/jdlugosz963/snorgd/internal/daemon"
	"github.com/jdlugosz963/snorgd/internal/dispatch"
	"github.com/jdlugosz963/snorgd/internal/dropbox"
	"github.com/jdlugosz963/snorgd/internal/formstate"
	"github.com/jdlugosz963/snorgd/internal/gitrepo"
	"github.com/jdlugosz963/snorgd/internal/ingest"
	"github.com/jdlugosz963/snorgd/internal/letter"
	"github.com/jdlugosz963/snorgd/internal/watch"
)

// eventBuffer lets a Dropbox sync burst queue up instead of throttling the watcher on
// an unbuffered channel. Without it the daemon would drain one note at a time and
// every "batch" would be a single note.
const eventBuffer = 64

func main() {
	log.SetFlags(log.LstdFlags)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "gen-templates":
			genTemplates(os.Args[2:])
		case "-h", "--help", "help":
			usage(os.Stdout)
		default:
			fmt.Fprintf(os.Stderr, "snorgd: unknown command %q\n\n", os.Args[1])
			usage(os.Stderr)
			os.Exit(2)
		}
		return
	}
	run()
}

func usage(w *os.File) {
	fmt.Fprint(w, `snorgd watches Dropbox and drives changed notes into a snorg archive.

usage:
  snorgd                 run the daemon (configured entirely by the environment)
  snorgd gen-templates   write the registered form templates into the current directory
`)
}

// run is the daemon.
func run() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// The archive must exist before anything reads it: the pipeline config lives
	// inside it by default, and migration walks it.
	repo, err := gitrepo.Ensure(gitrepo.Options{
		Dir:              cfg.Archive,
		Remote:           cfg.Remote,
		Name:             cfg.GitName,
		Email:            cfg.GitEmail,
		SSHKey:           cfg.SSHKey,
		SSHKeyPassphrase: cfg.SSHKeyPassphrase,
		KnownHosts:       cfg.SSHKnownHosts,
	})
	if err != nil {
		log.Fatalf("archive: %v", err)
	}

	pipeline, err := config.LoadFile(config.FilePath(cfg))
	if err != nil {
		log.Fatalf("pipeline config: %v", err)
	}

	ing, err := ingest.New(cfg.Archive)
	if err != nil {
		log.Fatalf("ingestor: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// snorg rejects notes written by an older schema, and that error surfaces from
	// every read. Upgrading up front is what makes an archive written by an earlier
	// snorg readable at all.
	upgraded, err := ing.Migrate(ctx)
	if err != nil {
		log.Fatalf("migrate archive: %v", err)
	}
	if len(upgraded) > 0 {
		log.Printf("snorgd: migrated %d page(s) to the current schema", len(upgraded))
	}

	dbx := dropbox.NewClient(cfg)
	phases := buildPhases(cfg, pipeline, ing, dbx)

	watchers := []watch.Watcher{dropbox.New(cfg, dbx)}
	events := make(chan watch.Event, eventBuffer)

	var wg sync.WaitGroup
	for _, w := range watchers {
		wg.Add(1)
		go func(w watch.Watcher) {
			defer wg.Done()
			if err := w.Watch(ctx, events); err != nil && ctx.Err() == nil {
				log.Printf("watcher %s stopped: %v", w.Name(), err)
				stop() // an unexpected watcher failure shuts the daemon down cleanly
			}
		}(w)
	}
	go func() { wg.Wait(); close(events) }()

	log.Printf("snorgd: watching %s -> %s", cfg.DropboxFolder, cfg.Archive)
	daemon.New(pipeline.Ingest.Rules, ing, phases, repo, pipeline.Pipeline.QuietPeriod.Duration()).Run(ctx, events)
	log.Printf("snorgd: shut down")
}

// buildPhases constructs the downstream stages, in the order a batch runs them:
// analyze, which transcribes the pages the tag rules select, then dispatch, which
// reads templated pages and routes the changed ones to their handlers. Analyzing
// first is what lets the dispatcher find a form's regions already transcribed.
func buildPhases(cfg *config.Config, pipeline *config.File, ing *ingest.Ingestor, dbx *dropbox.Client) []daemon.Phase {
	if !pipeline.Analyze.Enabled && !pipeline.Dispatch.Enabled {
		return nil
	}
	client := ing.Client()

	// One provider for every stage that wants one: credentials fail at startup
	// rather than on the first page, and an api_key_command runs once.
	var prov snorg.Provider
	if pipeline.Analyze.Enabled || pipeline.Letter.Enabled {
		p, err := client.NewProvider()
		if err != nil {
			log.Fatalf("provider: %v (analysis and the letter handler need a provider section in the archive's config.yaml)", err)
		}
		prov = p
	}

	var phases []daemon.Phase
	if pipeline.Analyze.Enabled {
		phases = append(phases, analyze.NewPhase(client, prov, pipeline.Analyze.Rules))
		log.Printf("snorgd: analyzing %s", describeRules(pipeline.Analyze.Rules))
	}
	if p := buildDispatch(cfg, pipeline, client, prov, dbx); p != nil {
		phases = append(phases, p)
	}
	return phases
}

// buildDispatch builds the dispatch stage, or nil when nothing would be routed.
func buildDispatch(cfg *config.Config, pipeline *config.File, client *snorg.Client, prov snorg.Provider, dbx *dropbox.Client) daemon.Phase {
	if !pipeline.Dispatch.Enabled {
		return nil
	}
	handlers := buildHandlers(client, prov, dbx, pipeline)
	if len(handlers) == 0 {
		log.Printf("snorgd: dispatch is enabled but every handler is off; no page will be routed")
		return nil
	}
	reg, err := dispatch.NewRegistry(handlers...)
	if err != nil {
		log.Fatalf("dispatch: %v", err)
	}

	// The form state sits in the state dir, outside the archive, so it is never
	// committed. Losing it simply has every page look new again, at the cost of
	// whatever its handler does — for the letter handler, a model call each.
	store, err := formstate.NewStore(filepath.Join(cfg.StateDir, "forms"))
	if err != nil {
		log.Fatalf("form state: %v", err)
	}

	phase, err := dispatch.NewPhase(snif.Open(client), client, store, reg, pipeline.Dispatch.Query)
	if err != nil {
		log.Fatalf("dispatch: %v", err)
	}
	log.Printf("snorgd: dispatching %s", strings.Join(reg.Names(), ", "))
	return phase
}

// describeRules renders the analyze selection for the startup log, so a running
// daemon says which pages it will spend model calls on.
func describeRules(set analyze.Set) string {
	if len(set) == 0 {
		return "every page"
	}
	parts := make([]string, len(set))
	for i, r := range set {
		tags := "any page"
		if len(r.Tags) > 0 {
			tags = strings.Join(r.Tags, "/")
		}
		if len(r.Exclude) > 0 {
			tags += " except " + strings.Join(r.Exclude, "/")
		}
		parts[i] = tags
	}
	return strings.Join(parts, ", ")
}

// buildHandlers builds every enabled handler. This is the list a new kind of page is
// added to — that, plus the handler itself, is the whole of it.
func buildHandlers(client *snorg.Client, prov snorg.Provider, dbx *dropbox.Client, pipeline *config.File) []dispatch.Handler {
	var handlers []dispatch.Handler
	if pipeline.Letter.Enabled {
		h, err := letter.New(letter.Deps{Archive: client, Provider: prov, Uploader: dbx}, pipeline.Letter)
		if err != nil {
			log.Fatalf("letter: %v", err)
		}
		handlers = append(handlers, h)
	}
	return handlers
}

// genTemplates draws the registered templates into the working directory.
//
// It deliberately needs no archive and no credentials — only the pipeline config that
// says what the forms offer — so a template can be drawn and looked at anywhere,
// including on a machine that has never seen the notes.
func genTemplates(args []string) {
	fs := flag.NewFlagSet("gen-templates", flag.ExitOnError)
	out := fs.String("o", ".", "directory to write into")
	force := fs.Bool("force", false, "overwrite existing files")
	printTeX := fs.Bool("print-tex", false, "write the embedded LaTeX template to stdout and exit")
	confPath := fs.String("c", "", "pipeline config (default: $SNORGD_CONFIG, else ./snorgd.yaml)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: snorgd gen-templates [-o dir] [-c config] [-force] [-print-tex]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *printTeX {
		if _, err := os.Stdout.Write(letter.LaTeXTemplate()); err != nil {
			log.Fatalf("gen-templates: %v", err)
		}
		return
	}

	path := *confPath
	if path == "" {
		if path = os.Getenv("SNORGD_CONFIG"); path == "" {
			path = "snorgd.yaml"
		}
	}
	pipeline, err := config.LoadFile(path)
	if err != nil {
		log.Fatalf("gen-templates: %v", err)
	}

	templates, err := declareTemplates(pipeline)
	if err != nil {
		log.Fatalf("gen-templates: %v", err)
	}
	if len(templates) == 0 {
		log.Fatalf("gen-templates: no handler is enabled in %s, so there is nothing to draw", path)
	}

	for _, t := range templates {
		written, err := dispatch.Write(t, *out, *force)
		if err != nil {
			log.Fatalf("gen-templates: %v", err)
		}
		for _, p := range written {
			fmt.Println(p)
		}
	}
	fmt.Fprintf(os.Stderr, "\nImport the .png on the device as a template, then put the .yaml in the\n"+
		"archive and include it from its config.yaml — snorg keeps only one templates:\n"+
		"list, so it must replace any that is there rather than sit beside it.\n")
}

// declareTemplates is the drawing half of buildHandlers: the same templates, from
// configuration alone.
func declareTemplates(pipeline *config.File) ([]dispatch.Template, error) {
	var out []dispatch.Template
	if pipeline.Letter.Enabled {
		t, err := letter.Declare(pipeline.Letter)
		if err != nil {
			return nil, fmt.Errorf("letter: %w", err)
		}
		out = append(out, t)
	}
	return out, nil
}
