// Command snorgd is a daemon that watches Dropbox for .note changes in real time,
// ingests each changed note into a local snorg archive (a clone of the notes repo),
// and commits+pushes that single change. Dropbox sits behind the watch.Watcher
// abstraction, so other note sources can be added without touching the daemon core.
package main

import (
	"context"
	"log"
	"os/signal"
	"sync"
	"syscall"

	"github.com/jdlugosz963/snorgd/internal/config"
	"github.com/jdlugosz963/snorgd/internal/daemon"
	"github.com/jdlugosz963/snorgd/internal/dropbox"
	"github.com/jdlugosz963/snorgd/internal/gitrepo"
	"github.com/jdlugosz963/snorgd/internal/ingest"
	"github.com/jdlugosz963/snorgd/internal/watch"
)

func main() {
	log.SetFlags(log.LstdFlags)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	repo, err := gitrepo.Ensure(cfg.Archive, cfg.Remote, cfg.GitName, cfg.GitEmail)
	if err != nil {
		log.Fatalf("archive: %v", err)
	}
	ing, err := ingest.New(cfg.Archive)
	if err != nil {
		log.Fatalf("ingestor: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	watchers := []watch.Watcher{dropbox.New(cfg)}
	events := make(chan watch.Event)

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
	daemon.New(ing, repo).Run(events)
	log.Printf("snorgd: shut down")
}
