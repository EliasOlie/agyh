package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/policy"
)

// Skill propagation: when a skill appears or changes in one agent and other
// agents lack that exact version, the bridge asks on chat whether to install
// it there. Nothing is installed without an answer (or ⚡, which approves
// future propagations until `/auto off`). Installs go through the same
// ledger and rules as `agyh skills link`.
//
// The first pass only records what exists: enabling propagation on a machine
// with dozens of skills must not flood the phone. `agyh skills link --all`
// covers those.

// PropagationSession is the approval session of skill propagation: ⚡ on a
// propagation prompt enables autonomous mode for it.
const PropagationSession = "skills-propagation"

// reaskAfter is how long an unanswered prompt waits before being asked again.
const reaskAfter = 24 * time.Hour

type propagationState struct {
	Baselined bool                     `json:"baselined"`
	Skills    map[string]propagationAt `json:"skills"` // by digest
}

type propagationAt struct {
	Name     string    `json:"name"`
	Decision string    `json:"decision"` // baseline, allowed, denied, unanswered, complete
	At       time.Time `json:"at"`
}

func (b *Bridge) loadPropagation() (*propagationState, error) {
	st := &propagationState{Skills: map[string]propagationAt{}}
	raw, err := os.ReadFile(b.propagationPath)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, err
	}
	if st.Skills == nil {
		st.Skills = map[string]propagationAt{}
	}
	return st, nil
}

func (b *Bridge) savePropagation(st *propagationState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(b.propagationPath), 0o700); err != nil {
		return err
	}
	tmp := b.propagationPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.propagationPath)
}

// PropagateSkills runs one pass. It is a no-op unless both manage_agents and
// auto_propagate are enabled. Prompts are asked one at a time.
func (b *Bridge) PropagateSkills(ctx context.Context) error {
	if !b.cfg.ManageAgents || !b.cfg.AutoPropagate || b.propagationPath == "" || b.ledgerPath == "" {
		return nil
	}
	if !b.propagating.TryLock() {
		return nil // a pass (maybe waiting on a prompt) is still running
	}
	defer b.propagating.Unlock()

	state, err := b.loadPropagation()
	if err != nil {
		return err
	}
	var (
		targets []agentkit.SkillTarget
		bySkill = map[string][]agentkit.Skill{} // digest -> installs
		order   []string
	)
	for _, h := range b.homes() {
		if !h.Available() {
			continue
		}
		if t, err := agentkit.Target(h); err == nil {
			targets = append(targets, t)
		}
		skills, _ := h.Skills()
		for _, s := range skills {
			if s.Scope == agentkit.ScopeBuiltin || s.Digest == "" {
				continue
			}
			if _, seen := bySkill[s.Digest]; !seen {
				order = append(order, s.Digest)
			}
			bySkill[s.Digest] = append(bySkill[s.Digest], s)
		}
	}

	if !state.Baselined {
		for d, installs := range bySkill {
			state.Skills[d] = propagationAt{Name: installs[0].Name, Decision: "baseline", At: time.Now().UTC()}
		}
		state.Baselined = true
		b.log.Info("skill propagation enabled: existing skills recorded, only new or changed ones will be offered", "skills", len(bySkill))
		return b.savePropagation(state)
	}

	known := map[string]bool{} // skill names seen before, to say "changed" vs "new"
	for _, p := range state.Skills {
		known[p.Name] = true
	}
	for _, digest := range order {
		installs := bySkill[digest]
		if prev, ok := state.Skills[digest]; ok && (prev.Decision != "unanswered" || time.Since(prev.At) < reaskAfter) {
			continue
		}
		src := installs[0]
		var missing []agentkit.SkillTarget
		for _, t := range targets {
			if !slices.ContainsFunc(installs, func(s agentkit.Skill) bool { return s.Agent == t.Name() }) {
				missing = append(missing, t)
			}
		}
		if len(missing) == 0 {
			state.Skills[digest] = propagationAt{Name: src.Name, Decision: "complete", At: time.Now().UTC()}
			continue
		}
		names := make([]string, len(missing))
		for i, t := range missing {
			names[i] = t.Name()
		}
		verb := "New skill"
		if known[src.Name] {
			verb = "Skill changed:"
		}
		source := src.Agent + " " + src.Scope
		if src.Plugin != "" {
			source += " " + src.Plugin
		}
		req := hook.Request{Agent: "agyh", SessionID: PropagationSession, Tool: "skills link",
			Summary: fmt.Sprintf("%s %s (from %s, %s). Install it into %s?", verb, src.Name, source, src.Hash, strings.Join(names, ", "))}
		d, _ := b.AskHuman(ctx, NewID(), req)
		now := time.Now().UTC()
		switch d.Verdict {
		case policy.Allow:
			state.Skills[digest] = propagationAt{Name: src.Name, Decision: "allowed", At: now}
			b.installSkill(ctx, src, source, missing, d.By)
		case policy.Deny:
			state.Skills[digest] = propagationAt{Name: src.Name, Decision: "denied", At: now}
		default:
			state.Skills[digest] = propagationAt{Name: src.Name, Decision: "unanswered", At: now}
		}
		if err := b.savePropagation(state); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return b.savePropagation(state)
}

// installSkill links src into the targets and reports the outcome on chat.
func (b *Bridge) installSkill(ctx context.Context, src agentkit.Skill, source string, targets []agentkit.SkillTarget, by string) {
	ledger, err := agentkit.LoadLedger(b.ledgerPath)
	var lines []string
	if err != nil {
		lines = append(lines, "ledger: "+err.Error())
	}
	for _, t := range targets {
		if ledger == nil {
			break
		}
		st, err := ledger.PlanLink(src, t, source)
		switch {
		case err != nil:
			lines = append(lines, fmt.Sprintf("✗ %s: %v", t.Name(), err))
		case st.Refused():
			lines = append(lines, fmt.Sprintf("✗ %s: %s exists and is not agyh's unedited copy: left untouched", t.Name(), hook.ShortenHome(st.Path)))
		case st.Kind == agentkit.StepPresent || st.Kind == agentkit.StepUpToDate:
			lines = append(lines, fmt.Sprintf("= %s: already there", t.Name()))
		default:
			if err := ledger.Apply(st); err != nil {
				lines = append(lines, fmt.Sprintf("✗ %s: %v", t.Name(), err))
			} else {
				lines = append(lines, fmt.Sprintf("✓ %s: %s", t.Name(), hook.ShortenHome(st.Path)))
			}
		}
	}
	b.log.Info("skill propagated", "skill", src.Name, "by", by)
	b.Broadcast(ctx, func(m Markup) string {
		head := "📦 " + m.Bold(m.Escape(src.Name))
		if by == "auto" {
			head += " (⚡ auto)"
		}
		return head + "\n" + m.Escape(strings.Join(lines, "\n"))
	})
}
