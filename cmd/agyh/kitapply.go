package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/sync"
)

func kitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kit",
		Short: "Put a restored kit back: instructions, memories, subagents, commands and project skills",
	}
	var (
		machine     string
		dry, asJSON bool
	)
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Write what this machine lacks from a restored kit (never replaces a file)",
		Long: `After ` + "`agyh sync restore`" + `, puts back what the kit carries besides
global skills and MCP servers:

  - global instructions: ~/.claude/CLAUDE.md, ~/.gemini/config/AGENTS.md,
    ~/.codex/AGENTS.md (and Claude Code output styles)
  - Claude Code memories (~/.claude/projects/*/memory), subagents and slash
    commands; Codex custom prompts
  - project skills (<workspace>/.claude/skills, <workspace>/.agents/skills),
    into workspaces that exist here: clone the repository first

Nothing that exists is replaced. A file that differs from the vault's is
reported with the path of the vault's copy, to compare by hand. Paths follow
a new home directory (another user name) automatically.

Global skills and MCP servers have their own commands:
  agyh skills link --all --from kit:<machine>
  agyh mcp apply`,
		Example: `  agyh sync restore --machine old-laptop
  agyh kit apply --machine old-laptop --dry-run
  agyh kit apply --machine old-laptop`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if machine == "" {
				cfg, err := config.Load()
				if err != nil {
					return err
				}
				machine = cfg.Sync.Machine
			}
			dir := sync.KitDir(machine)
			snap, err := sync.ReadKit(dir)
			if errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("no restored kit in %s: run `agyh sync restore --machine %s` first",
					shortPath(dir), sync.MachineName(machine))
			}
			if err != nil {
				return err
			}
			steps := sync.ApplyKit(dir, snap, dry)
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(steps)
			}
			return printKitSteps(cmd, steps, dry)
		},
	}
	apply.Flags().StringVar(&machine, "machine", "", "the restored kit to apply (default: this machine's)")
	apply.Flags().BoolVar(&dry, "dry-run", false, "show what would happen without writing anything")
	apply.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(apply)
	return cmd
}

func printKitSteps(cmd *cobra.Command, steps []sync.KitStep, dry bool) error {
	out := cmd.OutOrStdout()
	if len(steps) == 0 {
		fmt.Fprintln(out, "The kit has no instructions, memories, subagents, commands or project skills.")
		return nil
	}
	count := map[string]int{}
	order := map[string]int{sync.KitFailed: 0, sync.KitKept: 1, sync.KitNoWorkspace: 2, sync.KitRestored: 3, sync.KitWouldRestore: 3, sync.KitPresent: 4}
	sort.SliceStable(steps, func(i, j int) bool { return order[steps[i].Status] < order[steps[j].Status] })
	for _, s := range steps {
		count[s.Status]++
		if s.Status == sync.KitPresent {
			continue
		}
		mark := map[string]string{sync.KitRestored: "✓", sync.KitWouldRestore: "+", sync.KitKept: "≠", sync.KitNoWorkspace: "…", sync.KitFailed: "✗"}[s.Status]
		fmt.Fprintf(out, "%s %-13s %-6s %s\n", mark, s.Kind, s.Agent, hook.ShortenHome(s.Target))
		switch s.Status {
		case sync.KitKept:
			fmt.Fprintf(out, "    yours differs, kept; the vault's copy: %s\n", hook.ShortenHome(s.Detail))
		case sync.KitNoWorkspace:
			fmt.Fprintf(out, "    %s: %s\n", s.Status, s.Detail)
		case sync.KitFailed:
			fmt.Fprintf(out, "    %s\n", s.Detail)
		}
	}
	verb := "restored"
	if dry {
		verb = "would restore"
	}
	fmt.Fprintf(out, "\n%d %s, %d already here, %d kept (yours differs), %d waiting for their workspace",
		count[sync.KitRestored]+count[sync.KitWouldRestore], verb, count[sync.KitPresent], count[sync.KitKept], count[sync.KitNoWorkspace])
	if count[sync.KitFailed] > 0 {
		fmt.Fprintf(out, ", %d failed\n", count[sync.KitFailed])
		return fmt.Errorf("%d item(s) failed", count[sync.KitFailed])
	}
	fmt.Fprintln(out)
	return nil
}
