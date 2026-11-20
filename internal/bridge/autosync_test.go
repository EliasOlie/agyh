package bridge

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vaultsync "github.com/EliasOlie/agyh/internal/sync"
)

func TestAutoSync(t *testing.T) {
	b, _, _, _ := newBridge(t, 5)
	b.autoFirst, b.autoAfter = 10*time.Millisecond, 50*time.Millisecond
	ch := newFake("telegram")
	b.AddChannel(ch)
	var calls atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	b.vaultSync = func(context.Context) (vaultsync.SyncResult, error) {
		calls.Add(1)
		if fail.Load() {
			return vaultsync.SyncResult{}, errors.New("vault unreachable")
		}
		return vaultsync.SyncResult{Uploaded: 1}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	defer func() { cancel(); <-stopped }()
	go func() { b.runAutoSync(ctx); close(stopped) }()

	// The first sync runs soon after start; its failure is reported once.
	waitFor(t, func() bool { return calls.Load() == 1 })
	waitFor(t, func() bool { return strings.Contains(ch.last(), "vault unreachable") })

	// Sessions ending in a burst give one sync, after a quiet moment.
	for range 5 {
		b.kickSync()
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, func() bool { return calls.Load() == 2 })
	time.Sleep(120 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("a burst of session ends gave %d syncs", n-1)
	}
	ch.mu.Lock()
	reports := len(ch.sent)
	ch.mu.Unlock()
	if reports != 1 {
		t.Fatalf("a repeated failure was reported %d times", reports)
	}

	// Recovery is reported once too.
	fail.Store(false)
	b.kickSync()
	waitFor(t, func() bool { return strings.Contains(ch.last(), "works again") })
}

func TestAutoSyncSkipsWhileASyncRuns(t *testing.T) {
	b, _, _, _ := newBridge(t, 5)
	var calls atomic.Int32
	b.vaultSync = func(context.Context) (vaultsync.SyncResult, error) { calls.Add(1); return vaultsync.SyncResult{}, nil }
	b.syncing.Store(true) // a /sync from the phone
	b.autoSyncOnce(context.Background(), "interval")
	if calls.Load() != 0 {
		t.Fatal("ran two syncs at once")
	}
}
