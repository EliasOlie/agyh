package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

type Engine struct {
	cfg config.Sync
	st  *store.Store
	s3  *S3Client
}

type SyncResult struct {
	Uploaded   int       `json:"uploaded"`
	Downloaded int       `json:"downloaded"`
	Total      int       `json:"total"`
	Timestamp  time.Time `json:"timestamp"`
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

	decrypted, err := DecryptAndDecompress(data, e.cfg.Passphrase)
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

	encrypted, err := CompressAndEncrypt(raw, e.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("encrypt manifest: %w", err)
	}

	if err := e.s3.Put(ctx, e.manifestKey(), encrypted); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	return nil
}

// Diff compares local sessions and remote manifest sessions.
func (e *Engine) Diff(ctx context.Context) (Diff, *Manifest, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return Diff{}, nil, err
	}

	localSessions, err := e.st.AllSessions(ctx)
	if err != nil {
		return Diff{}, nil, fmt.Errorf("list local sessions: %w", err)
	}

	localMap := make(map[string]model.Session, len(localSessions))
	for _, s := range localSessions {
		localMap[s.Key()] = s
	}

	var toUpload []model.Session
	for _, local := range localSessions {
		rem, ok := m.Sessions[local.Key()]
		if !ok || local.UpdatedAt.After(rem.UpdatedAt) {
			toUpload = append(toUpload, local)
		}
	}

	var toDownload []SessionSummary
	for key, rem := range m.Sessions {
		local, ok := localMap[key]
		if !ok || rem.UpdatedAt.After(local.UpdatedAt) {
			toDownload = append(toDownload, rem)
		}
	}

	return Diff{ToUpload: toUpload, ToDownload: toDownload}, m, nil
}

// Status returns a high-level summary of the synchronization status.
func (e *Engine) Status(ctx context.Context) (StatusReport, error) {
	diff, m, err := e.Diff(ctx)
	if err != nil {
		return StatusReport{
			Enabled:  true,
			Endpoint: e.cfg.Endpoint,
			Bucket:   e.cfg.Bucket,
		}, err
	}

	localSessions, _ := e.st.AllSessions(ctx)
	return StatusReport{
		Enabled:        true,
		Endpoint:       e.cfg.Endpoint,
		Bucket:         e.cfg.Bucket,
		LocalSessions:  len(localSessions),
		RemoteSessions: len(m.Sessions),
		ToUpload:       len(diff.ToUpload),
		ToDownload:     len(diff.ToDownload),
		LastSynced:     m.UpdatedAt,
	}, nil
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

	encrypted, err := CompressAndEncrypt(raw, e.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("encrypt session bundle: %w", err)
	}

	return e.s3.Put(ctx, e.sessionKey(sess.Agent, sess.ID), encrypted)
}

// DownloadSession fetches, decrypts and stores a single remote session locally.
func (e *Engine) DownloadSession(ctx context.Context, summary SessionSummary) error {
	data, err := e.s3.Get(ctx, e.sessionKey(summary.Agent, summary.ID))
	if err != nil {
		return fmt.Errorf("get session blob %s: %w", summary.Key(), err)
	}

	decrypted, err := DecryptAndDecompress(data, e.cfg.Passphrase)
	if err != nil {
		return fmt.Errorf("decrypt session blob %s: %w", summary.Key(), err)
	}

	var bundle Bundle
	if err := json.Unmarshal(decrypted, &bundle); err != nil {
		return fmt.Errorf("decode session bundle %s: %w", summary.Key(), err)
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

	// Restore project associations
	for _, p := range bundle.Projects {
		note := bundle.Notes[p]
		_ = e.st.AddToProject(ctx, p, bundle.Session, note)
	}
	return nil
}

// Sync performs bidirectional synchronization.
func (e *Engine) Sync(ctx context.Context) (SyncResult, error) {
	diff, m, err := e.Diff(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	projectMap, notesMap, err := e.st.ProjectMap(ctx)
	if err != nil {
		return SyncResult{}, fmt.Errorf("read local projects: %w", err)
	}

	var (
		res SyncResult
		mu  sync.Mutex
	)

	// 1. Upload local changes
	for _, sess := range diff.ToUpload {
		if err := e.UploadSession(ctx, sess, projectMap, notesMap); err != nil {
			return res, fmt.Errorf("upload %s: %w", sess.Key(), err)
		}
		m.Sessions[sess.Key()] = SessionSummary{
			Agent:     sess.Agent,
			ID:        sess.ID,
			Title:     sess.Title,
			Workspace: sess.Workspace,
			StartedAt: sess.StartedAt,
			UpdatedAt: sess.UpdatedAt,
			Messages:  sess.Messages,
			Status:    sess.Status,
			Parent:    sess.Parent,
		}
		mu.Lock()
		res.Uploaded++
		mu.Unlock()
	}

	// 2. Download remote changes
	for _, summary := range diff.ToDownload {
		if err := e.DownloadSession(ctx, summary); err != nil {
			return res, fmt.Errorf("download %s: %w", summary.Key(), err)
		}
		mu.Lock()
		res.Downloaded++
		mu.Unlock()
	}

	// 3. Sync project metadata into manifest
	localProjects, _ := e.st.Projects(ctx)
	for _, lp := range localProjects {
		found := false
		for i, rp := range m.Projects {
			if rp.Name == lp.Name {
				found = true
				if lp.Description != "" && rp.Description == "" {
					m.Projects[i].Description = lp.Description
				}
				break
			}
		}
		if !found {
			m.Projects = append(m.Projects, lp)
		}
	}

	// 4. Save updated manifest to S3
	if res.Uploaded > 0 || res.Downloaded > 0 || len(diff.ToUpload) > 0 {
		if err := e.saveManifest(ctx, m); err != nil {
			return res, err
		}
	}

	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	return res, nil
}

// Push forces uploading all local sessions to the cloud vault.
func (e *Engine) Push(ctx context.Context) (SyncResult, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	localSessions, err := e.st.AllSessions(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	projectMap, notesMap, err := e.st.ProjectMap(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	var res SyncResult
	for _, sess := range localSessions {
		if err := e.UploadSession(ctx, sess, projectMap, notesMap); err != nil {
			return res, fmt.Errorf("upload %s: %w", sess.Key(), err)
		}
		m.Sessions[sess.Key()] = SessionSummary{
			Agent:     sess.Agent,
			ID:        sess.ID,
			Title:     sess.Title,
			Workspace: sess.Workspace,
			StartedAt: sess.StartedAt,
			UpdatedAt: sess.UpdatedAt,
			Messages:  sess.Messages,
			Status:    sess.Status,
			Parent:    sess.Parent,
		}
		res.Uploaded++
	}

	if err := e.saveManifest(ctx, m); err != nil {
		return res, err
	}

	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	return res, nil
}

// Pull downloads all remote sessions that are missing or newer.
func (e *Engine) Pull(ctx context.Context) (SyncResult, error) {
	m, err := e.fetchManifest(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	localSessions, err := e.st.AllSessions(ctx)
	if err != nil {
		return SyncResult{}, err
	}

	localMap := make(map[string]model.Session, len(localSessions))
	for _, s := range localSessions {
		localMap[s.Key()] = s
	}

	var res SyncResult
	for key, summary := range m.Sessions {
		local, ok := localMap[key]
		if !ok || summary.UpdatedAt.After(local.UpdatedAt) {
			if err := e.DownloadSession(ctx, summary); err != nil {
				return res, fmt.Errorf("download %s: %w", key, err)
			}
			res.Downloaded++
		}
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

	var res SyncResult
	for key, summary := range m.Sessions {
		if err := e.DownloadSession(ctx, summary); err != nil {
			return res, fmt.Errorf("restore %s: %w", key, err)
		}
		res.Downloaded++
	}

	res.Total = len(m.Sessions)
	res.Timestamp = time.Now()
	return res, nil
}
