package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/sync"
)

// mcpCandidate is an MCP server that can be copied, with its origin.
type mcpCandidate struct {
	name     string
	agent    string
	spec     agentkit.MCPSpec
	source   string
	warnings []string
}

// mcpSources lists MCP servers from every agent, one agent, or a restored kit.
func mcpSources(cfg *config.Config, from string) ([]mcpCandidate, error) {
	var entries []agentkit.MCPEntry
	label := func(e agentkit.MCPEntry) string {
		if e.Project != "" {
			return e.Agent + " " + e.Scope + ":" + shortPath(e.Project)
		}
		return e.Agent + " " + e.Scope
	}
	if from == "kit" || strings.HasPrefix(from, "kit:") {
		machine := strings.TrimPrefix(strings.TrimPrefix(from, "kit"), ":")
		if machine == "" {
			machine = cfg.Sync.Machine
		}
		machine = sync.MachineName(machine)
		dir := sync.KitDir(machine)
		files, _ := filepath.Glob(filepath.Join(dir, "*", "mcp.json"))
		if len(files) == 0 {
			return nil, fmt.Errorf("no restored kit for %q in %s: run `agyh sync restore --machine %s` first", machine, shortPath(dir), machine)
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				return nil, err
			}
			var doc struct{ Servers []agentkit.MCPEntry }
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, fmt.Errorf("%s: %w", shortPath(f), err)
			}
			entries = append(entries, doc.Servers...)
		}
		label = func(e agentkit.MCPEntry) string { return "kit:" + machine + " (" + e.Agent + ")" }
	} else {
		homes, err := kitHomes(from)
		if err != nil {
			return nil, err
		}
		for _, h := range homes {
			e, _ := h.MCPConfig()
			entries = append(entries, e...)
		}
	}
	var out []mcpCandidate
	for _, e := range entries {
		spec, warnings, err := agentkit.SpecFromEntry(e)
		if err != nil {
			continue // not something agyh can carry (reported by `agyh mcp`)
		}
		out = append(out, mcpCandidate{name: e.Name, agent: e.Agent, spec: spec, source: label(e), warnings: warnings})
	}
	return out, nil
}

func pickMCP(cands []mcpCandidate, name string) (mcpCandidate, error) {
	var matches []mcpCandidate
	for _, c := range cands {
		if c.name == name {
			matches = append(matches, c)
		}
	}
	if len(matches) == 0 {
		return mcpCandidate{}, fmt.Errorf("no MCP server named %q (see `agyh mcp`)", name)
	}
	digests := map[string]bool{}
	for _, m := range matches {
		digests[m.spec.Digest()] = true
	}
	if len(digests) > 1 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "%q is configured differently in %d places; pick one with --from:", name, len(matches))
		for _, m := range matches {
			fmt.Fprintf(&sb, "\n  %s  (%s)", m.source, m.spec.Summary())
		}
		return mcpCandidate{}, errors.New(sb.String())
	}
	return matches[0], nil
}

func mcpTarget(agent string) (agentkit.MCPTarget, error) {
	for _, h := range agentkit.All() {
		if h.Name() == agent {
			if !h.Available() {
				return nil, fmt.Errorf("%s is not installed on this machine", agent)
			}
			return agentkit.MCPTargetOf(h)
		}
	}
	return nil, fmt.Errorf("unknown agent %q (claude, agy, codex)", agent)
}

func describeMCP(st agentkit.MCPStep, dry bool) string {
	will := func(done, would string) string {
		if dry {
			return would
		}
		return done
	}
	switch st.Kind {
	case agentkit.StepInstall:
		return fmt.Sprintf("+ %s: %s %s (%s; from %s)", st.Agent, st.Name, will("added", "would be added"), st.Summary, st.Source)
	case agentkit.StepUpdate:
		return fmt.Sprintf("~ %s: %s %s (agyh's entry, unchanged since it wrote it; from %s)", st.Agent, st.Name, will("updated", "would be updated"), st.Source)
	case agentkit.StepRemove:
		return fmt.Sprintf("- %s: %s %s", st.Agent, st.Name, will("removed", "would be removed"))
	case agentkit.StepForget:
		return fmt.Sprintf("- %s: %s was already gone; %s", st.Agent, st.Name, will("record dropped", "record would be dropped"))
	case agentkit.StepUpToDate:
		return fmt.Sprintf("= %s: %s already up to date", st.Agent, st.Name)
	case agentkit.StepPresent:
		return fmt.Sprintf("= %s: %s already configured identically (not by agyh, left as is)", st.Agent, st.Name)
	case agentkit.StepUnmanaged:
		return fmt.Sprintf("✗ %s: a server named %s exists and was not added by agyh: left untouched", st.Agent, st.Name)
	case agentkit.StepModified:
		return fmt.Sprintf("✗ %s: %s was edited after agyh added it: left untouched", st.Agent, st.Name)
	}
	return st.Kind
}

func runMCPSteps(w io.Writer, ledger *agentkit.Ledger, steps []agentkit.MCPStep, dry bool) error {
	refused := 0
	for _, st := range steps {
		if st.Refused() {
			refused++
		} else if !dry {
			if err := ledger.ApplyMCP(st); err != nil {
				return fmt.Errorf("%s: %s: %w", st.Agent, st.Name, err)
			}
		}
		fmt.Fprintln(w, describeMCP(st, dry))
	}
	if dry {
		fmt.Fprintln(w, "(dry run: nothing was written)")
	}
	if refused > 0 {
		return fmt.Errorf("%d MCP server(s) left untouched to protect your config (see above)", refused)
	}
	return nil
}

func mcpApplyCmd() *cobra.Command {
	var (
		to       []string
		from     string
		all, dry bool
	)
	cmd := &cobra.Command{
		Use:   "apply <server>... | --all --from <source>",
		Short: "Add MCP servers to other agents, converting formats (tracked; never overwrites yours)",
		Long: `Copies MCP servers, secrets included, from one agent (or a restored kit) into
others, converting between their formats. Examples:

  agyh mcp apply github --to agy,codex          # github from wherever it is configured
  agyh mcp apply --all --from kit               # after a restore: every server back where it was
  agyh mcp apply janus --to claude --dry-run    # preview

How each agent is written (user scope):
  claude  through its own CLI (claude mcp add-json/remove): ~/.claude.json is
          rewritten by Claude Code all the time and is never edited directly
  agy     ~/.gemini/config/mcp_config.json, other servers and keys kept, backup next to it
  codex   a marked block appended to config.toml (your comments and layout are
          kept), removed exactly by agyh mcp remove. Untested against a real Codex.

Rules: a server agyh did not add is never replaced, nor one it added that you
edited since; every change is recorded (agyh mcp links) and can be undone
(agyh mcp remove). Settings with no equivalent in the target agent are
listed, never silently dropped.

` + manageAgentsHelp,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !all && len(args) == 0 {
				return errors.New("name the servers to apply, or use --all with --from")
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
			cands, err := mcpSources(cfg, from)
			if err != nil {
				return err
			}
			var picked []mcpCandidate
			if all {
				picked = cands
			} else {
				for _, name := range args {
					c, err := pickMCP(cands, name)
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
			w := cmd.OutOrStdout()
			var steps []agentkit.MCPStep
			for _, c := range picked {
				agents := to
				if len(agents) == 0 || (len(agents) == 1 && agents[0] == "original") {
					if !strings.HasPrefix(c.source, "kit:") {
						return errors.New("say where to add them with --to (e.g. --to agy,codex)")
					}
					agents = []string{c.agent}
				}
				for _, w2 := range c.warnings {
					fmt.Fprintf(w, "! %s (from %s): %s\n", c.name, c.source, w2)
				}
				for _, agent := range agents {
					target, err := mcpTarget(agent)
					if err != nil {
						return err
					}
					st, err := ledger.PlanMCP(c.name, c.spec, target, c.source)
					if err != nil {
						return fmt.Errorf("%s: %w", c.name, err)
					}
					steps = append(steps, st)
				}
			}
			if len(steps) == 0 {
				fmt.Fprintln(w, "Nothing to do.")
				return nil
			}
			return runMCPSteps(w, ledger, steps, dry)
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "agents to add them to (claude, agy, codex), or \"original\" for kit servers")
	cmd.Flags().StringVar(&from, "from", "", "where to copy from: an agent, kit or kit:<machine> (default: every agent)")
	cmd.Flags().BoolVar(&all, "all", false, "every server of --from")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would happen without writing anything")
	return cmd
}

func mcpRemoveCmd() *cobra.Command {
	var (
		agent string
		dry   bool
	)
	cmd := &cobra.Command{
		Use:   "remove <server>...",
		Short: "Remove MCP servers agyh added (only its own, unedited entries)",
		Long:  "Removes MCP servers that agyh added with agyh mcp apply. An entry you edited since is left untouched.\n\n" + manageAgentsHelp,
		Args:  cobra.MinimumNArgs(1),
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
			var steps []agentkit.MCPStep
			for _, name := range args {
				found := false
				for _, l := range ledger.MCP {
					if l.Name != name || (agent != "" && l.Agent != agent) {
						continue
					}
					found = true
					target, err := mcpTarget(l.Agent)
					if err != nil {
						return err
					}
					st, err := ledger.PlanMCPRemove(l, target)
					if err != nil {
						return err
					}
					steps = append(steps, st)
				}
				if !found {
					return fmt.Errorf("agyh has not added an MCP server named %q (see `agyh mcp links`)", name)
				}
			}
			return runMCPSteps(cmd.OutOrStdout(), ledger, steps, dry)
		},
	}
	cmd.Flags().StringVarP(&agent, "agent", "a", "", "only from this agent")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "show what would happen without writing anything")
	return cmd
}

func mcpLinksCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "links",
		Short: "List the MCP servers agyh added to agents, and whether they were edited since",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ledger, err := agentkit.LoadLedger(ledgerPath())
			if err != nil {
				return err
			}
			type row struct {
				agentkit.MCPLink
				Status string `json:"status"` // ok, edited, missing, unknown
			}
			var rows []row
			for _, l := range ledger.MCP {
				status := "unknown"
				if target, err := mcpTarget(l.Agent); err == nil {
					if st, err := ledger.PlanMCPRemove(l, target); err == nil {
						status = map[string]string{agentkit.StepRemove: "ok", agentkit.StepModified: "edited", agentkit.StepForget: "missing"}[st.Kind]
					}
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
				fmt.Fprintln(w, "agyh has not added any MCP server. See `agyh mcp apply --help`.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tSERVER\tSTATUS\tFROM")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Agent, r.Name, r.Status, r.Source)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
