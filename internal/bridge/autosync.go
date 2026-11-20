package bridge

import (
	"context"
	"time"
)

// Automatic vault sync: the vault is only a backup if it is current. The
// bridge syncs every sync.interval (default 6h) and, debounced, a few
// minutes after an agent session ends (the Stop hook's notification), so a
// day's work is in the vault without anyone remembering /sync.
//
// It is quiet when things work: the phone hears about a failure once, and
// once more when syncing works again.

var (
	autoSyncFirst = time.Minute     // first sync after the bridge starts
	autoSyncAfter = 3 * time.Minute // after a session ends (debounced)
)

func (b *Bridge) runAutoSync(ctx context.Context) {
	b.log.Info("automatic vault sync on", "every", b.cfg.Sync.SyncInterval().String())
	periodic := time.NewTimer(autoSyncFirst)
	defer periodic.Stop()
	var debounce *time.Timer
	var debounced <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.syncKick:
			if debounce == nil {
				debounce = time.NewTimer(autoSyncAfter)
				debounced = debounce.C
			} else {
				debounce.Reset(autoSyncAfter)
			}
		case <-debounced:
			debounce, debounced = nil, nil
			b.autoSyncOnce(ctx, "session ended")
		case <-periodic.C:
			b.autoSyncOnce(ctx, "interval")
			periodic.Reset(b.cfg.Sync.SyncInterval())
		}
	}
}

// kickSync asks for a sync soon (after autoSyncAfter of quiet).
func (b *Bridge) kickSync() {
	if b.syncKick == nil {
		return
	}
	select {
	case b.syncKick <- struct{}{}:
	default: // one is already pending
	}
}

func (b *Bridge) autoSyncOnce(ctx context.Context, why string) {
	if !b.syncing.CompareAndSwap(false, true) {
		return // a /sync is running
	}
	defer b.syncing.Store(false)
	sctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := b.vaultSync(sctx)
	if err != nil {
		b.log.Warn("automatic sync failed", "why", why, "err", err)
		if !b.syncFailing.Swap(true) {
			b.Broadcast(ctx, func(m Markup) string {
				return "⚠️ Automatic vault sync failed / Sincronização automática falhou: " + m.Escape(err.Error()) +
					"\n\nI'll keep trying and tell you when it works again. / Vou continuar tentando."
			})
		}
		return
	}
	b.log.Info("automatic sync", "why", why, "uploaded", res.Uploaded, "downloaded", res.Downloaded, "total", res.Total)
	if b.syncFailing.Swap(false) {
		b.Broadcast(ctx, func(Markup) string {
			return "☁️ Vault sync works again / Sincronização voltou a funcionar."
		})
	}
}
