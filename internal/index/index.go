// Package index keeps the store in sync with every agent's history,
// re-parsing only the sessions whose files changed since the last run.
package index

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"

	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

// Stats summarizes one sync.
type Stats struct {
	Agent     string `json:"agent"`
	Available bool   `json:"available"`
	Seen      int    `json:"seen"`
	Updated   int    `json:"updated"`
	Removed   int    `json:"removed"`
	Errors    int    `json:"errors"`
}

// Sync indexes every available source. With full=true every session is re-parsed.
func Sync(ctx context.Context, st *store.Store, sources []source.Source, full bool) ([]Stats, error) {
	var all []Stats
	for _, src := range sources {
		s, err := syncOne(ctx, st, src, full)
		if err != nil {
			return all, fmt.Errorf("%s: %w", src.Name(), err)
		}
		all = append(all, s)
	}
	return all, nil
}

func syncOne(ctx context.Context, st *store.Store, src source.Source, full bool) (Stats, error) {
	stats := Stats{Agent: src.Name(), Available: src.Available()}
	if !stats.Available {
		return stats, nil
	}
	refs, err := src.Refs(ctx)
	if err != nil {
		return stats, err
	}
	stats.Seen = len(refs)
	known, err := st.Mtimes(ctx, src.Name())
	if err != nil {
		return stats, err
	}

	keep := make(map[string]bool, len(refs))
	var todo []source.Ref
	for _, r := range refs {
		keep[r.Path] = true
		if m, ok := known[r.Path]; full || !ok || m != r.ModTime.UnixNano() {
			todo = append(todo, r)
		}
	}

	// Hot sessions first: if indexing is interrupted (or huge on first run),
	// the recent days are already searchable.
	sort.Slice(todo, func(i, j int) bool { return todo[i].ModTime.After(todo[j].ModTime) })

	// Parse in parallel (I/O + JSON bound), write in batches from one goroutine.
	jobs := make(chan source.Ref)
	results := make(chan store.Item)
	var errCount int
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := min(runtime.NumCPU(), 8)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				sess, msgs, err := src.Load(ctx, r)
				if err != nil {
					mu.Lock()
					errCount++
					mu.Unlock()
					continue
				}
				results <- store.Item{Session: sess, Messages: msgs}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, r := range todo {
			select {
			case jobs <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	batch := make([]store.Item, 0, 64)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := st.UpsertMany(ctx, batch); err != nil {
			return err
		}
		stats.Updated += len(batch)
		batch = batch[:0]
		return nil
	}
	var writeErr error
	for it := range results {
		if writeErr != nil {
			continue // drain so workers can exit
		}
		batch = append(batch, it)
		if len(batch) == cap(batch) {
			writeErr = flush()
		}
	}
	if writeErr == nil {
		writeErr = flush()
	}
	if writeErr != nil {
		return stats, writeErr
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	stats.Errors = errCount
	stats.Removed, err = st.DeleteMissing(ctx, src.Name(), keep)
	return stats, err
}
