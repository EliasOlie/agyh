package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/store"
	"github.com/EliasOlie/agyh/internal/sync"
)

const manageAgentsHelp = `agyh writes into your agents' skill folders only when you allow it:
add "manage_agents": true to ` + "`~/.config/agyh/config.json`" + ` (or use --dry-run to preview).`

func ledgerPath() string { return filepath.Join(store.DataDir(), "links.json") }

// candidate is a skill that can be copied, with a human label of its origin.
type candidate struct {
	skill  agentkit.Skill
	source string
}

// skillSources lists copyable skills: from every agent, one agent, or a
// restored kit ("kit" = this machine's, "kit:<machine>"). Skills bundled with
// an agent are never copied.
func skillSources(cfg *config.Config, from string) ([]candidate, error) {
	label := func(s agentkit.Skill) string {
		out := s.Agent + " " + s.Scope
		if s.Plugin != "" {
			out += " " + s.Plugin
		}
		return out
	}
	var out []candidate
	if from == "kit" || strings.HasPrefix(from, "kit:") {
		machine := strings.TrimPrefix(strings.TrimPrefix(from, "kit"), ":")
		if machine == "" {
			machine = cfg.Sync.Machine
		}
		machine = sync.MachineName(machine)
		skills, errs := agentkit.ScanKit(sync.KitDir(machine), machine)
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		if len(skills) == 0 {
			return nil, fmt.Errorf("no restored kit for %q in %s: run `agyh sync restore --machine %s` first", machine, shortPath(sync.KitDir(machine)), machine)
		}
		for _, s := range skills {
			out = append(out, candidate{s, "kit:" + machine + " (" + s.Agent + ")"})
		}
		return out, nil
	}
	homes, err := kitHomes(from)
	if err != nil {
		return nil, err
	}
	for _, h := range homes {
		skills, _ := h.Skills()
		for _, s := range skills {
			if s.Scope != agentkit.ScopeBuiltin {
				out = append(out, candidate{s, label(s)})
			}
		}
	}
	return out, nil
}

// pickSkill finds one skill by name (display name or folder name). Copies
// with different content are ambiguous: the user must say which with --from.
func pickSkill(cands []candidate, name string) (candidate, error) {
	var matches []candidate
	for _, c := range cands {
		if c.skill.Name == name || filepath.Base(c.skill.Path) == name {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return candidate{}, fmt.Errorf("no skill named %q (see `agyh skills`)", name)
	}
	digests := map[string]bool{}
	for _, m := range matches {
		digests[m.skill.Digest] = true
	}
	if len(digests) > 1 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "%q has %d different versions; pick one with --from:", name, len(digests))
		for _, m := range matches {
			fmt.Fprintf(&sb, "\n  %s  %s", m.skill.Hash, m.source)
		}
		return candidate{}, errors.New(sb.String())
	}
	return matches[0], nil
}

func targetsFor(to []string, c candidate) ([]string, error) {
	if len(to) == 0 || (len(to) == 1 && to[0] == "original") {
		if c.skill.Scope != "kit" {
			return nil, errors.New("say where to install with --to (e.g. --to claude,codex)")
		}
		return []string{c.skill.Agent}, nil
	}
	return to, nil
}

func targetHome(agent string) (agentkit.SkillTarget, error) {
	for _, h := range agentkit.All() {
		if h.Name() == agent {
			t, err := agentkit.Target(h)
			if err != nil {
				return nil, err
			}
			if !h.Available() {
				return nil, fmt.Errorf("%s is not installed on this machine", agent)
			}
			return t, nil
		}
	}
	return nil, fmt.Errorf("unknown agent %q (claude, codex)", agent)
}

// describe renders a step for humans; dry runs use the conditional.
func describe(st agentkit.Step, dry bool) string {
	where := shortPath(st.Path)
	will := func(done, would string) string {
		if dry {
			return would
		}
		return done
	}
	switch st.Kind {
	case agentkit.StepInstall:
		return fmt.Sprintf("+ %s: %s %s %s (from %s)", st.Agent, st.Name, will("installed in", "would be installed in"), where, st.Source)
	case agentkit.StepUpdate:
		return fmt.Sprintf("~ %s: %s %s (agyh's copy, unchanged since it installed it; from %s)", st.Agent, st.Name, will("updated", "would be updated"), st.Source)
	case agentkit.StepRemove:
		return fmt.Sprintf("- %s: %s %s from %s", st.Agent, st.Name, will("removed", "would be removed"), where)
	case agentkit.StepForget:
		return fmt.Sprintf("- %s: %s was already gone from %s; %s", st.Agent, st.Name, where, will("record dropped", "record would be dropped"))
	case agentkit.StepUpToDate:
		return fmt.Sprintf("= %s: %s already up to date", st.Agent, st.Name)
	case agentkit.StepPresent:
		return fmt.Sprintf("= %s: %s already there, identical (not installed by agyh, left as is)", st.Agent, st.Name)
	case agentkit.StepUnmanaged:
		return fmt.Sprintf("✗ %s: %s exists and was not created by agyh: left untouched", st.Agent, where)
	case agentkit.StepModified:
		return fmt.Sprintf("✗ %s: %s was edited after agyh installed it: left untouched", st.Agent, where)
	}
	return st.Kind
}

// runSteps prints and (unless dry) applies steps. Refusals are reported as an error at the end.
func runSteps(w io.Writer, ledger *agentkit.Ledger, steps []agentkit.Step, dry bool) error {
	refused := 0
	for _, st := range steps {
		if st.Refused() {
			refused++
		} else if !dry {
			if err := ledger.Apply(st); err != nil {
				return fmt.Errorf("%s: %s: %w", st.Agent, st.Name, err)
			}
		}
		fmt.Fprintln(w, describe(st, dry))
	}
	if dry {
		fmt.Fprintln(w, "(dry run: nothing was written)")
	}
	if refused > 0 {
		return fmt.Errorf("%d skill folder(s) left untouched to protect your files (see above)", refused)
	}
	return nil
}

func skillsLinkCmd() *cobra.Command {
	var (
		to       []string
		from     string
		all, dry bool
	)
	cmd := &cobra.Command{
		Use:   "link <skill>... | --all --from <source>",
		Short: "Install skills into other agents (copies, tracked; never overwrites your files)",
		Long: `Copies a skill from wherever it is installed into other agents, so every agent
has it. Examples:

  agyh skills link ponytail --to claude,codex      # ponytail from Antigravity into Claude and Codex
  agyh skills link --all --from kit                # after a restore: every skill back into its agent
  agyh skills link seo --to codex --dry-run        # preview

Sources (--from): an agent (claude, agy, codex), "kit" (this machine's
restored kit) or "kit:<machine>". Without --from, every agent is searched.
Kit skills default to the agent they came from; otherwise --to is required.

Rules: a folder that agyh did not create is never touched, nor one it created
that you edited since. Every install is recorded (agyh skills links) and can
be undone (agyh skills unlink). Antigravity can be a source, not a target: it
has no documented folder for user skills.

` + manageAgentsHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all && len(args) == 0 {
				return errors.New("name the skills to link, or use --all with --from")
			}
			if all && from == "" {
				return errors.New("--all needs --from (an agent, kit or kit:<machine>)")
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if !dry && !cfg.ManageAgents {
				return errors.New(manageAgentsHelp)
			}
			cands, err := skillSources(cfg, from)
			if err != nil {
				return err
			}
			var picked []candidate
			if all {
				picked = cands
			} else {
				for _, name := range args {
					c, err := pickSkill(cands, name)
					if err != nil {
						return err
					}
					picked = append(picked, c)
				}
			}
			ledger, err := agentkit.LoadLedger(ledgerPath())
			if err != nil {
				return err
			}
			var steps []agentkit.Step
			for _, c := range picked {
				agents, err := targetsFor(to, c)
				if err != nil {
					return err
				}
				for _, agent := range agents {
					target, err := targetHome(agent)
					if err != nil {
						return err
					}
					st, err := ledger.PlanLink(c.skill, target, c.source)
					if err != nil {
						return fmt.Errorf("%s: %w", c.skill.Name, err)
					}
					steps = append(steps, st)
				}
			}
			if len(steps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing to do.")
				return nil
			}
			return runSteps(cmd.OutOrStdout(), ledger, steps, dry)
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "agents to install into (claude, codex), or \"original\" for kit skills")
	cmd.Flags().StringVar(&from, "from", "", "where to copy from: an agent, kit or kit:<machine> (default: every agent)")
	cmd.Flags().BoolVar(&all, "all", false, "every skill of --from")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would happen without writing anything")
	return cmd
}

func skillsUnlinkCmd() *cobra.Command {
	var (
		agent string
		dry   bool
	)
	cmd := &cobra.Command{
		Use:   "unlink <skill>...",
		Short: "Remove skills agyh installed (only agyh's own, unedited copies)",
		Long: `Removes skill folders that agyh installed with agyh skills link. A folder you
edited since is left untouched, and folders agyh did not create are never
considered.

` + manageAgentsHelp,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if !dry && !cfg.ManageAgents {
				return errors.New(manageAgentsHelp)
			}
			ledger, err := agentkit.LoadLedger(ledgerPath())
			if err != nil {
				return err
			}
			var steps []agentkit.Step
			for _, name := range args {
				found := false
				for _, l := range ledger.Links {
					if (l.Name == name || filepath.Base(l.Path) == name) && (agent == "" || l.Agent == agent) {
						steps = append(steps, ledger.PlanUnlink(l))
						found = true
					}
				}
				if !found {
					return fmt.Errorf("agyh has not installed a skill named %q%s (see `agyh skills links`)", name, map[bool]string{true: " in " + agent, false: ""}[agent != ""])
				}
			}
			return runSteps(cmd.OutOrStdout(), ledger, steps, dry)
		},
	}
	cmd.Flags().StringVarP(&agent, "agent", "a", "", "only from this agent")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would happen without writing anything")
	return cmd
}

func skillsLinksCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "links",
		Short: "List the skills agyh installed into agents, and whether they were edited since",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ledger, err := agentkit.LoadLedger(ledgerPath())
			if err != nil {
				return err
			}
			type row struct {
				agentkit.Link
				Status string `json:"status"` // ok, edited, missing
			}
			var rows []row
			for _, l := range ledger.Links {
				status := "ok"
				switch ledger.PlanUnlink(l).Kind {
				case agentkit.StepForget:
					status = "missing"
				case agentkit.StepModified:
					status = "edited"
				}
				rows = append(rows, row{l, status})
			}
			slices.SortFunc(rows, func(a, b row) int { return strings.Compare(a.Agent+a.Name, b.Agent+b.Name) })
			w := cmd.OutOrStdout()
			if asJSON {
				if rows == nil {
					rows = []row{}
				}
				return printJSON(w, rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(w, "agyh has not installed any skill. See `agyh skills link --help`.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tSKILL\tSTATUS\tPATH\tFROM")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Agent, r.Name, r.Status, shortPath(r.Path), r.Source)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
