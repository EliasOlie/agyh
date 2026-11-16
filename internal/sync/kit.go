package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/store"
)

// The kit is a machine's agent environment: the skills users installed in
// each agent, the plugins they use (as references) and their MCP servers,
// secrets included. Everything is encrypted with the vault key.
//
//	<prefix>/kit/skills/<digest>.blob    one skill folder (tar), content-addressed:
//	                                     identical skills are stored once
//	<prefix>/kit/machines/<name>.blob    one machine's snapshot
//	<prefix>/kit/index.blob              machines and stored skill digests
//
// Snapshots are per machine, not merged: restoring brings back exactly the
// environment that machine had. Restore writes only into agyh's own data
// directory; installing into agents is a separate, explicit step.

const kitSnapshotVersion = 1

// KitSkill is one skill in a snapshot. Its content is the blob named by Digest.
type KitSkill struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent"`
	Scope       string `json:"scope"`
	Digest      string `json:"digest"`
	Path        string `json:"path"` // where it lived on the machine
}

// KitSnapshot is one machine's agent environment.
type KitSnapshot struct {
	Version   int                  `json:"version"`
	Machine   string               `json:"machine"`
	CreatedAt time.Time            `json:"created_at"`
	Skills    []KitSkill           `json:"skills"`
	Plugins   []agentkit.PluginRef `json:"plugins,omitempty"`
	MCP       []agentkit.MCPEntry  `json:"mcp,omitempty"` // secrets included: never print
}

// KitMachine summarizes one machine's latest snapshot.
type KitMachine struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
	Skills    int       `json:"skills"`
	MCP       int       `json:"mcp"`
	Plugins   int       `json:"plugins"`
}

type kitIndex struct {
	Machines map[string]KitMachine `json:"machines"`
	// Digests of skill blobs already stored, so a sync uploads only new
	// skills without a HEAD per skill. Losing an entry only costs a re-upload.
	Skills map[string]bool `json:"skills"`
}

// KitResult reports a kit backup or restore.
type KitResult struct {
	Machine   string   `json:"machine"`
	Skills    int      `json:"skills"`
	NewSkills int      `json:"new_skills,omitempty"` // uploaded by this backup
	MCP       int      `json:"mcp"`
	Plugins   int      `json:"plugins"`
	Dir       string   `json:"dir,omitempty"` // restore destination
	Warnings  []string `json:"warnings,omitempty"`
}

var ErrNoKit = errors.New("no kit snapshot for this machine")

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// MachineName is sync.machine, else the hostname, made safe for a key.
func MachineName(configured string) string {
	name := configured
	if name == "" {
		name, _ = os.Hostname()
	}
	name = strings.Trim(unsafeName.ReplaceAllString(name, "-"), "-.")
	if name == "" {
		name = "machine"
	}
	return name
}

func (e *Engine) kitKey(parts ...string) string {
	return path.Join(append([]string{e.cfg.Prefix, "kit"}, parts...)...)
}

func (e *Engine) machine() string { return MachineName(e.cfg.Machine) }

func (e *Engine) homes() []agentkit.Home {
	if e.kitHomes != nil {
		return e.kitHomes()
	}
	return agentkit.All()
}

// getSealed fetches and decrypts one JSON object; found is false if missing.
func (e *Engine) getSealed(ctx context.Context, key string, v any) (bool, error) {
	data, err := e.s3.Get(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	keys, err := e.ready(ctx, false)
	if err != nil {
		return false, err
	}
	plain, err := keys.open(data)
	if err != nil {
		return false, fmt.Errorf("decrypt %s: %w", key, err)
	}
	return true, json.Unmarshal(plain, v)
}

func (e *Engine) putSealed(ctx context.Context, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	keys, err := e.ready(ctx, true)
	if err != nil {
		return err
	}
	sealed, err := keys.seal(raw)
	if err != nil {
		return err
	}
	return e.s3.Put(ctx, key, sealed)
}

func (e *Engine) kitIndex(ctx context.Context) (*kitIndex, error) {
	idx := &kitIndex{}
	if _, err := e.getSealed(ctx, e.kitKey("index.blob"), idx); err != nil {
		return nil, fmt.Errorf("kit index: %w", err)
	}
	if idx.Machines == nil {
		idx.Machines = map[string]KitMachine{}
	}
	if idx.Skills == nil {
		idx.Skills = map[string]bool{}
	}
	return idx, nil
}

// KitMachines lists the machines that have a kit snapshot, newest first.
func (e *Engine) KitMachines(ctx context.Context) ([]KitMachine, error) {
	idx, err := e.kitIndex(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]KitMachine, 0, len(idx.Machines))
	for _, m := range idx.Machines {
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b KitMachine) int { return b.UpdatedAt.Compare(a.UpdatedAt) })
	return out, nil
}

// PushKit snapshots this machine's agent environment into the vault:
// user-installed skills (content), plugin references and MCP servers.
// Skills bundled with an agent or shipped inside plugins are not copied:
// the agent or the plugin brings them back.
func (e *Engine) PushKit(ctx context.Context) (KitResult, error) {
	if _, err := e.fetchManifest(ctx); err != nil { // proves the passphrase before any write
		return KitResult{}, err
	}
	res := KitResult{Machine: e.machine()}
	snap := KitSnapshot{Version: kitSnapshotVersion, Machine: res.Machine, CreatedAt: time.Now().UTC()}
	warnf := func(format string, a ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, a...)) }

	for _, h := range e.homes() {
		if !h.Available() {
			continue
		}
		skills, errs := h.Skills()
		for _, err := range errs {
			warnf("%s skills: %v", h.Name(), err)
		}
		for _, s := range skills {
			if s.Scope != agentkit.ScopeUser {
				continue
			}
			if s.Digest == "" {
				warnf("%s skill %s not backed up: too large", h.Name(), s.Name)
				continue
			}
			snap.Skills = append(snap.Skills, KitSkill{Name: s.Name, Description: s.Description, Agent: s.Agent,
				Scope: s.Scope, Digest: s.Digest, Path: s.Path})
		}
		mcp, errs := h.MCPConfig()
		for _, err := range errs {
			warnf("%s mcp: %v", h.Name(), err)
		}
		snap.MCP = append(snap.MCP, mcp...)
		plugins, errs := h.Plugins()
		for _, err := range errs {
			warnf("%s plugins: %v", h.Name(), err)
		}
		snap.Plugins = append(snap.Plugins, plugins...)
	}

	idx, err := e.kitIndex(ctx)
	if err != nil {
		return res, err
	}

	// Upload each new skill once. Pack re-derives the digest from the bytes
	// it archived; if a file changed since listing, the snapshot follows.
	var (
		mu       sync.Mutex
		uploaded atomic.Int64
		todo     []int
		seen     = map[string]bool{}
	)
	for i, s := range snap.Skills {
		if !idx.Skills[s.Digest] && !seen[s.Digest] {
			seen[s.Digest] = true
			todo = append(todo, i)
		}
	}
	err = each(ctx, len(todo), func(ctx context.Context, n int) error {
		s := &snap.Skills[todo[n]]
		archive, digest, err := agentkit.Pack(s.Path)
		if err != nil {
			return fmt.Errorf("pack skill %s: %w", s.Name, err)
		}
		keys, err := e.ready(ctx, true)
		if err != nil {
			return err
		}
		sealed, err := keys.seal(archive)
		if err != nil {
			return err
		}
		if err := e.s3.Put(ctx, e.kitKey("skills", digest+".blob"), sealed); err != nil {
			return fmt.Errorf("upload skill %s: %w", s.Name, err)
		}
		mu.Lock()
		idx.Skills[digest] = true
		if digest != s.Digest {
			res.Warnings = append(res.Warnings, fmt.Sprintf("skill %s changed during backup; stored its latest content", s.Name))
			for i := range snap.Skills {
				if snap.Skills[i].Digest == s.Digest {
					snap.Skills[i].Digest = digest
				}
			}
		}
		mu.Unlock()
		uploaded.Add(1)
		return nil
	})
	res.NewSkills = int(uploaded.Load())
	if err != nil {
		// Record what did upload, then fail: the snapshot would point at missing blobs.
		_ = e.putSealed(ctx, e.kitKey("index.blob"), idx)
		return res, err
	}

	if err := e.putSealed(ctx, e.kitKey("machines", res.Machine+".blob"), &snap); err != nil {
		return res, fmt.Errorf("save kit snapshot: %w", err)
	}
	res.Skills, res.MCP, res.Plugins = len(snap.Skills), len(snap.MCP), len(snap.Plugins)
	idx.Machines[res.Machine] = KitMachine{Name: res.Machine, UpdatedAt: snap.CreatedAt,
		Skills: res.Skills, MCP: res.MCP, Plugins: res.Plugins}
	if err := e.putSealed(ctx, e.kitKey("index.blob"), idx); err != nil {
		return res, fmt.Errorf("save kit index: %w", err)
	}
	return res, nil
}

// KitDir is where a machine's kit is restored: inside agyh's own data dir.
func KitDir(machine string) string {
	return filepath.Join(store.DataDir(), "kit", MachineName(machine))
}

// RestoreKit downloads a machine's snapshot ("" = this machine) into
// KitDir(machine), never into an agent's configuration:
//
//	<dir>/<agent>/skills/<name>/   each skill, verified against its digest
//	<dir>/<agent>/mcp.json         MCP servers, secrets included (mode 0600)
//	<dir>/plugins.json             plugins to reinstall
//	<dir>/kit.json                 the snapshot without MCP secrets
func (e *Engine) RestoreKit(ctx context.Context, machine string) (KitResult, error) {
	if machine == "" {
		machine = e.machine()
	}
	machine = MachineName(machine)
	var snap KitSnapshot
	found, err := e.getSealed(ctx, e.kitKey("machines", machine+".blob"), &snap)
	if err != nil {
		return KitResult{}, err
	}
	if !found {
		return KitResult{Machine: machine}, fmt.Errorf("%w %q", ErrNoKit, machine)
	}
	if snap.Version > kitSnapshotVersion {
		return KitResult{}, fmt.Errorf("kit snapshot format %d is newer than this agyh supports: upgrade agyh", snap.Version)
	}
	dir := KitDir(machine)
	res := KitResult{Machine: machine, Dir: dir, MCP: len(snap.MCP), Plugins: len(snap.Plugins)}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}

	keys, err := e.ready(ctx, false)
	if err != nil {
		return res, err
	}
	var (
		used    = map[string]bool{}
		targets = make([]string, len(snap.Skills))
	)
	for i, s := range snap.Skills {
		name := s.Name
		if unsafeName.MatchString(name) || name == "" || name == "." || name == ".." {
			name = "skill-" + s.Digest[:12]
		}
		t := filepath.Join(dir, MachineName(s.Agent), "skills", name)
		if used[t] { // two skills with one name in one agent
			t += "-" + s.Digest[:12]
		}
		used[t], targets[i] = true, t
	}
	var restored atomic.Int64
	err = each(ctx, len(snap.Skills), func(ctx context.Context, i int) error {
		s := snap.Skills[i]
		data, err := e.s3.Get(ctx, e.kitKey("skills", s.Digest+".blob"))
		if err != nil {
			return fmt.Errorf("skill %s: %w", s.Name, err)
		}
		archive, err := keys.open(data)
		if err != nil {
			return fmt.Errorf("skill %s: %w", s.Name, err)
		}
		if err := agentkit.Unpack(archive, targets[i], s.Digest); err != nil {
			return fmt.Errorf("skill %s: %w", s.Name, err)
		}
		restored.Add(1)
		return nil
	})
	res.Skills = int(restored.Load())
	if err != nil {
		return res, err
	}

	byAgent := map[string][]agentkit.MCPEntry{}
	for _, m := range snap.MCP {
		byAgent[m.Agent] = append(byAgent[m.Agent], m)
	}
	for agent, entries := range byAgent {
		if err := writePrivateJSON(filepath.Join(dir, MachineName(agent), "mcp.json"), map[string]any{"servers": entries}); err != nil {
			return res, err
		}
	}
	if err := writePrivateJSON(filepath.Join(dir, "plugins.json"), map[string]any{"plugins": snap.Plugins}); err != nil {
		return res, err
	}
	public := snap
	public.MCP = nil
	return res, writePrivateJSON(filepath.Join(dir, "kit.json"), public)
}

// writePrivateJSON writes v atomically with owner-only permissions.
func writePrivateJSON(p string, v any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
