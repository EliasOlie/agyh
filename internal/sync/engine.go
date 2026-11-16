package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

type Engine struct {
	cfg config.Sync
	st  *store.Store
	s3  *S3Client

	keyMu  sync.Mutex
	keys   *vaultKeys
	header *vaultHeader // nil until the vault has a v2 header
}

type SyncResult struct {
	Uploaded   int       `json:"uploaded"`
	Downloaded int       `json:"downloaded"`
	Total      int       `json:"total"`
	Timestamp  time.Time `json:"timestamp"`
	// LegacyBlobs: some sessions are still encrypted with the v1 key; run `agyh sync migrate`.
	LegacyBlobs bool `json:"legacy_blobs,omitempty"`
}

func NewEngine(cfg config.Sync, st *store.Store) (*Engine, error) {
	if !cfg.Enabled {
		return nil, errors.New("sync is disabled: configure s3 endpoint, bucket and credentials in config.json")
	}
	s3Client, err := NewS3Client(S3Config{
		Endpoint:  cfg.Endpoint,
		Bucket:    cfg.Bucket,
		Region:    cfg.Region,
		AccessKey: cfg.AccessKey,
		SecretKey: cfg.SecretKey,
	})
	if err != nil {
		return nil, err
	}
	return &Engine{
		cfg: cfg,
		st:  st,
		s3:  s3Client,
	}, nil
}

func (e *Engine) manifestKey() string {
	return path.Join(e.cfg.Prefix, "manifest.blob")
}

func (e *Engine) headerKey() string {
	return path.Join(e.cfg.Prefix, "vault.json")
}

func (e *Engine) fetchHeader(ctx context.Context) (*vaultHeader, error) {
	data, err := e.s3.Get(ctx, e.headerKey())
	if errors.Is(err, ErrObjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch vault header: %w", err)
	}
	var h vaultHeader
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrVaultHeader, err)
	}
	return &h, nil
}

// ready returns the vault keys. Reads work without a v2 header (v1 vaults);
// when write is set, a missing header is created first, so every new blob is v2.
func (e *Engine) ready(ctx context.Context, write bool) (*vaultKeys, error) {
	e.keyMu.Lock()
	defer e.keyMu.Unlock()
	if e.keys != nil && (e.keys.hasHeader || !write) {
		return e.keys, nil
	}
	h, err := e.fetchHeader(ctx)
	if err != nil {
		return nil, err
	}
	if h == nil && write {
		if h, err = e.createHeader(ctx); err != nil {
			return nil, err
		}
	}
	k := &vaultKeys{passphrase: e.cfg.Passphrase}
	if h != nil {
		if k.v2, err = h.key(e.cfg.Passphrase); err != nil {
			return nil, err
		}
		k.hasHeader = true
	}
	e.keys, e.header = k, h
	return k, nil
}

// createHeader adds a v2 header with a fresh random salt. On a v1 vault the
// passphrase must first open the existing manifest: a header written with a
// wrong passphrase would lock everyone else out. Concurrent creators race on
// a conditional write and everyone adopts whatever header is stored.
func (e *Engine) createHeader(ctx context.Context) (*vaultHeader, error) {
	legacy := false
	data, err := e.s3.Get(ctx, e.manifestKey())
	switch {
	case err == nil:
		if _, err := DecryptAndDecompress(data, e.cfg.Passphrase); err != nil {
			return nil, fmt.Errorf("decrypt manifest: %w", err)
		}
		legacy = true
	case !errors.Is(err, ErrObjectNotFound):
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	h, err := newVaultHeader(e.cfg.Passphrase, legacy)
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := e.s3.PutIfAbsent(ctx, e.headerKey(), raw); err != nil {
		return nil, fmt.Errorf("create vault header: %w", err)
	}
	stored, err := e.fetchHeader(ctx)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, errors.New("vault header missing right after creating it")
	}
	return stored, nil
}

// legacy reports whether v1-encrypted blobs may remain in the vault.
func (e *Engine) legacy(m *Manifest) bool {
	e.keyMu.Lock()
	defer e.keyMu.Unlock()
	if e.header == nil {
		return len(m.Sessions) > 0
	}
	return e.header.LegacyBlobs
}

func (e *Engine) sessionKey(agent, id string) string {
	return path.Join(e.cfg.Prefix, "sessions", agent, id+".blob")
}

func (e *Engine) fetchManifest(ctx context.Context) (*Manifest, error) {
	data, err := e.s3.Get(ctx, e.manifestKey())
	if errors.Is(err, ErrObjectNotFound) {
		return NewManifest(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}

	keys, err := e.ready(ctx, false)
	if err != nil {
		return nil, err
	}
	decrypted, err := keys.open(data)
	if err != nil {
		return nil, fmt.Errorf("decrypt manifest: %w", err)
	}

	var m Manifest
	if err := json.Unmarshal(decrypted, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if m.Sessions == nil {
		m.Sessions = make(map[string]SessionSummary)
	}
	if m.ProjectSessions == nil {
		m.ProjectSessions = make(map[string][]string)
	}
	if m.Notes == nil {
		m.Notes = make(map[string]map[string]string)
	}
	return &m, nil
}

func (e *Engine) saveManifest(ctx context.Context, m *Manifest) error {
	m.UpdatedAt = time.Now()
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	keys, err := e.ready(ctx, true)
	if err != nil {
		return err
	}
	encrypted, err := keys.seal(raw)
	if err != nil {
		return fmt.Errorf("encrypt manifest: %w", err)
	}

	if err := e.s3.Put(ctx, e.manifestKey(), encrypted); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}

// syncWorkers bounds concurrent vault transfers: enough to hide S3 round-trip
// latency, few enough to stay polite to a small self-hosted S3.
const syncWorkers = 6

// metaHash fingerprints a session's project memberships and notes, so that
// saving a session into a project or editing a note reaches the vault even
// though the session itself did not change. "" means "no projects", which is
// also what manifests written before this field was used contain.
func metaHash(projects []string, notes map[string]string) string {
	if len(projects) == 0 {
		return ""
	}
	ps := slices.Compact(slices.Sorted(slices.Values(projects)))
	h := sha256.New()
	for _, p := range ps {
		fmt.Fprintf(h, "%q=%q\n", p, notes[p])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func summaryOf(s model.Session, hash string) SessionSummary {
	return SessionSummary{
		Agent: s.Agent, ID: s.ID, Title: s.Title, Workspace: s.Workspace,
		StartedAt: s.StartedAt, UpdatedAt: s.UpdatedAt, Messages: s.Messages,
		Status: s.Status, Parent: s.Parent, Hash: hash,
	}
}

// localState is a snapshot of the local index used to plan a sync.
type localState struct {
	sessions []model.Session
	byKey    map[string]model.Session
	projects map[string][]string
	notes    map[string]map[string]string
}

func (e *Engine) local(ctx context.Context) (*localState, error) {
	sessions, err := e.st.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list local sessions: %w", err)
	}
	projects, notes, err := e.st.ProjectMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("read local projects: %w", err)
	}
	l := &localState{sessions: sessions, byKey: make(map[string]model.Session, len(sessions)),
		projects: projects, notes: notes}
	for _, s := range sessions {
		l.byKey[s.Key()] = s
	}
	return l, nil
}

func (l *localState) hash(key string) string { return metaHash(l.projects[key], l.notes[key]) }

// Diff compares local sessions and remote manifest sessions.
//
//   - A newer side wins: newer local sessions are uploaded, newer remote ones downloaded.
//   - With equal timestamps, differing project fingerprints are reconciled:
//     remote memberships are merged in (ToMerge, never removing one) and
//     a local side that has projects is uploaded so the vault gets the union.
func (e *Engine) Diff(ctx context.Context) (Diff, *Manifest, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return Diff{}, nil, err
	}
	l, err := e.local(ctx)
	if err != nil {
		return Diff{}, nil, err
	}
	return planDiff(m, l), m, nil
}

func planDiff(m *Manifest, l *localState) Diff {
	d := Diff{LocalSessions: len(l.sessions)}
	for _, local := range l.sessions {
		key := local.Key()
		rem, ok := m.Sessions[key]
		lh := l.hash(key)
		switch {
		case !ok, local.UpdatedAt.After(rem.UpdatedAt):
			d.ToUpload = append(d.ToUpload, local)
		case local.UpdatedAt.Equal(rem.UpdatedAt) && lh != rem.Hash:
			if lh != "" {
				d.ToUpload = append(d.ToUpload, local)
			}
			if rem.Hash != "" {
				d.ToMerge = append(d.ToMerge, rem)
			}
		}
	}
	for key, rem := range m.Sessions {
		if local, ok := l.byKey[key]; !ok || rem.UpdatedAt.After(local.UpdatedAt) {
			d.ToDownload = append(d.ToDownload, rem)
		}
	}
	return d
}

// Status returns a high-level summary of the synchronization status.
func (e *Engine) Status(ctx context.Context) (StatusReport, error) {
	report := StatusReport{Enabled: true, Endpoint: e.cfg.Endpoint, Bucket: e.cfg.Bucket}
	diff, m, err := e.Diff(ctx)
	if err != nil {
		return report, err
	}
	report.LocalSessions = diff.LocalSessions
	report.RemoteSessions = len(m.Sessions)
	report.ToUpload = len(diff.ToUpload)
	report.ToDownload = len(diff.ToDownload) + len(diff.ToMerge)
	report.LastSynced = m.UpdatedAt
	report.LegacyEncryption = e.legacy(m)
	return report, nil
}

// UploadSession packages, encrypts and uploads a single session.
func (e *Engine) UploadSession(ctx context.Context, sess model.Session, projectMap map[string][]string, notesMap map[string]map[string]string) error {
	_, msgs, err := source.LoadSessionOrArchive(ctx, sess)
	if err != nil {
		// Even if messages cannot be read (e.g. truncated file), upload metadata with empty messages
		msgs = nil
	}

	key := sess.Key()
	bundle := Bundle{
		Session:  sess,
		Messages: msgs,
		Projects: projectMap[key],
		Notes:    notesMap[key],
	}

	raw, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("encode session bundle: %w", err)
	}

	keys, err := e.ready(ctx, true)
	if err != nil {
		return err
	}
	encrypted, err := keys.seal(raw)
	if err != nil {
		return fmt.Errorf("encrypt session bundle: %w", err)
	}

	return e.s3.Put(ctx, e.sessionKey(sess.Agent, sess.ID), encrypted)
}

func (e *Engine) fetchBundle(ctx context.Context, summary SessionSummary) (Bundle, error) {
	data, err := e.s3.Get(ctx, e.sessionKey(summary.Agent, summary.ID))
	if err != nil {
		return Bundle{}, fmt.Errorf("get session blob %s: %w", summary.Key(), err)
	}
	keys, err := e.ready(ctx, false)
	if err != nil {
		return Bundle{}, err
	}
	decrypted, err := keys.open(data)
	if err != nil {
		return Bundle{}, fmt.Errorf("decrypt session blob %s: %w", summary.Key(), err)
	}
	var bundle Bundle
	if err := json.Unmarshal(decrypted, &bundle); err != nil {
		return Bundle{}, fmt.Errorf("decode session bundle %s: %w", summary.Key(), err)
	}
	return bundle, nil
}

// DownloadSession fetches, decrypts and stores a single remote session locally.
func (e *Engine) DownloadSession(ctx context.Context, summary SessionSummary) error {
	bundle, err := e.fetchBundle(ctx, summary)
	if err != nil {
		return err
	}

	// Save messages to local archive directory
	archPath, err := source.SaveArchive(bundle.Session, bundle.Messages)
	if err != nil {
		return fmt.Errorf("save archive %s: %w", summary.Key(), err)
	}
	bundle.Session.Path = "archive:" + archPath

	// Upsert session into SQLite database and FTS5 index
	if err := e.st.Upsert(ctx, bundle.Session, bundle.Messages); err != nil {
		return fmt.Errorf("upsert store %s: %w", summary.Key(), err)
	}
	return e.applyProjects(ctx, bundle)
}

// mergeProjects adds a remote session's project memberships and notes to the
// local index without touching the session itself.
func (e *Engine) mergeProjects(ctx context.Context, summary SessionSummary) error {
	bundle, err := e.fetchBundle(ctx, summary)
	if err != nil {
		return err
	}
	return e.applyProjects(ctx, bundle)
}

// applyProjects only ever adds memberships (a non-empty remote note replaces
// the local one). A project name the local validator rejects is skipped rather
// than failing the whole sync; database errors are returned.
func (e *Engine) applyProjects(ctx context.Context, bundle Bundle) error {
	for _, p := range bundle.Projects {
		if store.ValidProjectName(p) != nil {
			continue
		}
		if err := e.st.AddToProject(ctx, p, bundle.Session, bundle.Notes[p]); err != nil {
			return fmt.Errorf("restore project %s for %s: %w", p, bundle.Session.Key(), err)
		}
	}
	return nil
}

// each runs fn for every index in [0, n) on up to syncWorkers goroutines.
// One failing transfer does not cancel the others: every item is attempted,
// successes are kept, and all errors are returned joined. Cancelling ctx
// (Ctrl+C) still stops the remaining work.
func each(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	var (
		g    errgroup.Group
		mu   sync.Mutex
		errs []error
	)
	g.SetLimit(syncWorkers)
	for i := range n {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if err := fn(ctx, i); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// uploadAll uploads sessions concurrently and records each success in the
// manifest, so a partial failure still leaves a manifest worth saving.
func (e *Engine) uploadAll(ctx context.Context, m *Manifest, sessions []model.Session, l *localState) (int, error) {
	var mu sync.Mutex
	n := 0
	err := each(ctx, len(sessions), func(ctx context.Context, i int) error {
		s := sessions[i]
		if err := e.UploadSession(ctx, s, l.projects, l.notes); err != nil {
			return fmt.Errorf("upload %s: %w", s.Key(), err)
		}
		mu.Lock()
		m.Sessions[s.Key()] = summaryOf(s, l.hash(s.Key()))
		n++
		mu.Unlock()
		return nil
	})
	return n, err
}

func countEach(ctx context.Context, items []SessionSummary, fn func(context.Context, SessionSummary) error) (int, error) {
	var n atomic.Int64
	err := each(ctx, len(items), func(ctx context.Context, i int) error {
		if err := fn(ctx, items[i]); err != nil {
			return err
		}
		n.Add(1)
		return nil
	})
	return int(n.Load()), err
}

// mergeProjectList adds local projects missing from the manifest and fills
// empty descriptions. It reports whether the manifest changed.
func mergeProjectList(m *Manifest, local []store.Project) bool {
	changed := false
	for _, lp := range local {
		i := slices.IndexFunc(m.Projects, func(rp store.Project) bool { return rp.Name == lp.Name })
		switch {
		case i < 0:
			m.Projects = append(m.Projects, lp)
			changed = true
		case lp.Description != "" && m.Projects[i].Description == "":
			m.Projects[i].Description = lp.Description
			changed = true
		}
	}
	return changed
}

// Sync performs bidirectional synchronization. Remote changes are applied
// first, so the uploads that follow carry the merged project memberships.
func (e *Engine) Sync(ctx context.Context) (SyncResult, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	l, err := e.local(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	diff := planDiff(m, l)

	var res SyncResult
	if res.Downloaded, err = countEach(ctx, diff.ToDownload, func(ctx context.Context, s SessionSummary) error {
		if err := e.DownloadSession(ctx, s); err != nil {
			return fmt.Errorf("download %s: %w", s.Key(), err)
		}
		return nil
	}); err != nil {
		return res, err
	}
	if _, err = countEach(ctx, diff.ToMerge, func(ctx context.Context, s SessionSummary) error {
		if err := e.mergeProjects(ctx, s); err != nil {
			return fmt.Errorf("merge projects %s: %w", s.Key(), err)
		}
		return nil
	}); err != nil {
		return res, err
	}

	if len(diff.ToDownload)+len(diff.ToMerge) > 0 {
		if l, err = e.local(ctx); err != nil {
			return res, err
		}
	}
	// A merge may have made both sides equal: skip uploads that would change nothing.
	toUpload := slices.DeleteFunc(diff.ToUpload, func(s model.Session) bool {
		rem, ok := m.Sessions[s.Key()]
		return ok && s.UpdatedAt.Equal(rem.UpdatedAt) && l.hash(s.Key()) == rem.Hash
	})
	var uploadErr error
	res.Uploaded, uploadErr = e.uploadAll(ctx, m, toUpload, l)

	localProjects, err := e.st.Projects(ctx)
	if err != nil {
		return res, errors.Join(uploadErr, fmt.Errorf("read local projects: %w", err))
	}
	if mergeProjectList(m, localProjects) || res.Uploaded > 0 {
		if err := e.saveManifest(ctx, m); err != nil {
			return res, errors.Join(uploadErr, err)
		}
	}
	if uploadErr != nil {
		return res, uploadErr
	}
	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	res.LegacyBlobs = e.legacy(m)
	return res, nil
}

// Push forces uploading all local sessions to the cloud vault.
func (e *Engine) Push(ctx context.Context) (SyncResult, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	l, err := e.local(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	var res SyncResult
	var uploadErr error
	res.Uploaded, uploadErr = e.uploadAll(ctx, m, l.sessions, l)
	if res.Uploaded > 0 || uploadErr == nil {
		if err := e.saveManifest(ctx, m); err != nil {
			return res, errors.Join(uploadErr, err)
		}
	}
	if uploadErr != nil {
		return res, uploadErr
	}
	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	res.LegacyBlobs = e.legacy(m)
	return res, nil
}

// Pull downloads all remote sessions that are missing or newer, and merges
// project memberships changed on other machines. It never uploads.
func (e *Engine) Pull(ctx context.Context) (SyncResult, error) {
	diff, m, err := e.Diff(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	var res SyncResult
	if res.Downloaded, err = countEach(ctx, diff.ToDownload, func(ctx context.Context, s SessionSummary) error {
		if err := e.DownloadSession(ctx, s); err != nil {
			return fmt.Errorf("download %s: %w", s.Key(), err)
		}
		return nil
	}); err != nil {
		return res, err
	}
	if _, err = countEach(ctx, diff.ToMerge, func(ctx context.Context, s SessionSummary) error {
		if err := e.mergeProjects(ctx, s); err != nil {
			return fmt.Errorf("merge projects %s: %w", s.Key(), err)
		}
		return nil
	}); err != nil {
		return res, err
	}
	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	return res, nil
}

// Restore downloads ALL sessions from the cloud vault, rebuilding the local store.
func (e *Engine) Restore(ctx context.Context) (SyncResult, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	all := slices.Collect(maps.Values(m.Sessions))
	var res SyncResult
	if res.Downloaded, err = countEach(ctx, all, func(ctx context.Context, s SessionSummary) error {
		if err := e.DownloadSession(ctx, s); err != nil {
			return fmt.Errorf("restore %s: %w", s.Key(), err)
		}
		return nil
	}); err != nil {
		return res, err
	}
	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	return res, nil
}

// Migrate re-encrypts every v1 session blob with the vault's own Argon2id key,
// rewrites the manifest as v2 and clears the header's legacy flag. It is
// idempotent and safe to interrupt: v1 and v2 blobs stay readable side by
// side, and a re-run only rewrites what is still v1. Uploaded counts the
// sessions rewritten.
func (e *Engine) Migrate(ctx context.Context) (SyncResult, error) {
	m, err := e.fetchManifest(ctx) // proves the passphrase before anything is written
	if err != nil {
		return SyncResult{}, err
	}
	keys, err := e.ready(ctx, true)
	if err != nil {
		return SyncResult{}, err
	}
	all := slices.Collect(maps.Values(m.Sessions))
	var rewritten atomic.Int64
	err = each(ctx, len(all), func(ctx context.Context, i int) error {
		s := all[i]
		key := e.sessionKey(s.Agent, s.ID)
		data, err := e.s3.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("get %s: %w", s.Key(), err)
		}
		if !bytes.HasPrefix(data, magicEncrypted) {
			return nil // already v2 (or a plain-mode blob)
		}
		plain, err := keys.open(data)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", s.Key(), err)
		}
		sealed, err := keys.seal(plain)
		if err != nil {
			return err
		}
		if err := e.s3.Put(ctx, key, sealed); err != nil {
			return fmt.Errorf("rewrite %s: %w", s.Key(), err)
		}
		rewritten.Add(1)
		return nil
	})
	res := SyncResult{Uploaded: int(rewritten.Load())}
	if err != nil {
		return res, err
	}
	if err := e.saveManifest(ctx, m); err != nil {
		return res, err
	}
	e.keyMu.Lock()
	h := *e.header
	e.keyMu.Unlock()
	if h.LegacyBlobs {
		h.LegacyBlobs = false
		raw, err := json.MarshalIndent(&h, "", "  ")
		if err != nil {
			return res, err
		}
		if err := e.s3.Put(ctx, e.headerKey(), raw); err != nil {
			return res, fmt.Errorf("update vault header: %w", err)
		}
		e.keyMu.Lock()
		e.header.LegacyBlobs = false
		e.keyMu.Unlock()
	}
	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	return res, nil
}
