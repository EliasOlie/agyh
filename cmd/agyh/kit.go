package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/model"
)

const codexNote = "Codex support follows OpenAI's public docs and has not been tested against a real Codex install: " +
	"if it misreads yours, please open an issue or, better, a PR with a fixture."

// kitHomes returns the agents to inspect: available ones, optionally one only.
func kitHomes(agent string) ([]agentkit.Home, error) {
	var out []agentkit.Home
	for _, h := range agentkit.All() {
		if agent != "" && h.Name() != agent {
			continue
		}
		if h.Available() {
			out = append(out, h)
		}
	}
	if agent != "" && len(out) == 0 {
		for _, h := range agentkit.All() {
			if h.Name() == agent {
				return nil, nil // known agent, not installed: empty listing
			}
		}
		return nil, fmt.Errorf("unknown agent %q (claude, agy, codex)", agent)
	}
	return out, nil
}

func warn(w io.Writer, errs []error) {
	for _, err := range errs {
		fmt.Fprintln(w, "agyh: warning:", err)
	}
}

func skillsCmd() *cobra.Command {
	var (
		agent       string
		asJSON, all bool
	)
	cmd := &cobra.Command{
		Use:     "skills",
		Aliases: []string{"skill"},
		Short:   "List the Agent Skills installed in each coding agent (read-only)",
		Long: `Lists every Agent Skill (a folder with a SKILL.md) installed in Claude Code,
Antigravity and Codex, grouped by name, so you can see which agents have
which skills and whether their copies differ. Nothing is modified.

Skills bundled with an agent itself are hidden unless --all is given.

` + codexNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			homes, err := kitHomes(agent)
			if err != nil {
				return err
			}
			var (
				skills []agentkit.Skill
				errs   []error
			)
			for _, h := range homes {
				s, e := h.Skills()
				skills, errs = append(skills, s...), append(errs, e...)
			}
			if !all {
				skills = slices.DeleteFunc(skills, func(s agentkit.Skill) bool { return s.Scope == agentkit.ScopeBuiltin })
			}
			warn(cmd.ErrOrStderr(), errs)
			groups := agentkit.GroupSkills(skills)
			if asJSON {
				if groups == nil {
					groups = []agentkit.SkillGroup{}
				}
				return printJSON(cmd.OutOrStdout(), groups)
			}
			return printSkills(cmd.OutOrStdout(), groups, homeNames(homes))
		},
	}
	cmd.Flags().StringVarP(&agent, "agent", "a", "", "only this agent (claude, agy, codex)")
	cmd.Flags().BoolVar(&all, "all", false, "include skills bundled with the agents themselves")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(skillsLinkCmd(), skillsUnlinkCmd(), skillsLinksCmd(), skillsSearchCmd(), skillsInstallCmd())
	return cmd
}

func homeNames(homes []agentkit.Home) []string {
	out := make([]string, len(homes))
	for i, h := range homes {
		out[i] = h.Name()
	}
	return out
}

// cell describes how one agent has a skill or server: its scope, "off" when
// disabled, "-" when absent.
func cell[T any](items []T, agent string, get func(T) (string, string, bool)) string {
	var parts []string
	for _, it := range items {
		a, scope, disabled := get(it)
		if a != agent {
			continue
		}
		if disabled {
			scope += " (off)"
		}
		parts = append(parts, scope)
	}
	if len(parts) == 0 {
		return "-"
	}
	slices.Sort(parts)
	return strings.Join(slices.Compact(parts), ",")
}

func printSkills(w io.Writer, groups []agentkit.SkillGroup, agents []string) error {
	if len(groups) == 0 {
		fmt.Fprintln(w, "No skills found.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "SKILL\t%s\tNOTE\n", strings.ToUpper(strings.Join(agents, "\t")))
	everywhere, differ := 0, 0
	for _, g := range groups {
		row := []string{g.Name}
		for _, a := range agents {
			row = append(row, cell(g.Installs, a, func(s agentkit.Skill) (string, string, bool) {
				return s.Agent, s.Scope, s.Disabled
			}))
		}
		var notes []string
		var plugins []string
		for _, s := range g.Installs {
			if s.Plugin != "" {
				plugins = append(plugins, s.Plugin)
			}
		}
		if len(plugins) > 0 {
			slices.Sort(plugins)
			notes = append(notes, "from "+strings.Join(slices.Compact(plugins), ", "))
		}
		if g.Versions > 1 {
			differ++
			notes = append(notes, fmt.Sprintf("%d different versions", g.Versions))
		}
		if len(g.Agents()) == len(agents) {
			everywhere++
		}
		row = append(row, strings.Join(notes, "; "))
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\n%d skills · %d in every agent · %d with differing copies\n", len(groups), everywhere, differ)
	return nil
}

func mcpCmd() *cobra.Command {
	var (
		agent  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "List the MCP servers configured in each coding agent (read-only, secrets never shown)",
		Long: `Lists the MCP servers configured in Claude Code, Antigravity and Codex,
grouped by name, so you can see which agents have which servers and whether
they point at the same thing. Nothing is modified.

Only non-secret facts are shown: the command name (never its arguments), the
URL scheme and host (never its path or query) and the names of environment
variables (never their values). Header values are never read out.

` + codexNote,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			homes, err := kitHomes(agent)
			if err != nil {
				return err
			}
			var (
				servers []agentkit.MCPServer
				errs    []error
			)
			for _, h := range homes {
				s, e := h.MCPServers()
				servers, errs = append(servers, s...), append(errs, e...)
			}
			warn(cmd.ErrOrStderr(), errs)
			groups := agentkit.GroupMCP(servers)
			if asJSON {
				if groups == nil {
					groups = []agentkit.MCPGroup{}
				}
				return printJSON(cmd.OutOrStdout(), groups)
			}
			return printMCP(cmd.OutOrStdout(), groups, homeNames(homes))
		},
	}
	cmd.Flags().StringVarP(&agent, "agent", "a", "", "only this agent (claude, agy, codex)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(mcpApplyCmd(), mcpRemoveCmd(), mcpLinksCmd())
	return cmd
}

func printMCP(w io.Writer, groups []agentkit.MCPGroup, agents []string) error {
	if len(groups) == 0 {
		fmt.Fprintln(w, "No MCP servers found.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "SERVER\t%s\tTRANSPORT\tTARGET\tNOTE\n", strings.ToUpper(strings.Join(agents, "\t")))
	for _, g := range groups {
		row := []string{g.Name}
		for _, a := range agents {
			row = append(row, cell(g.Servers, a, func(s agentkit.MCPServer) (string, string, bool) {
				scope := s.Scope
				if s.Project != "" {
					scope += ":" + model.Truncate(shortPath(s.Project), 24)
				}
				return s.Agent, scope, s.Disabled
			}))
		}
		first := g.Servers[0]
		transport, target, note := first.Transport, first.Target, ""
		if g.Differs {
			transport, target = "", strings.Join(g.Targets(), " | ")
			note = "differs between agents"
		}
		row = append(row, transport, target, note)
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}
