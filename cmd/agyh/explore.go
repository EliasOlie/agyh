package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/agentkit"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/explore"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/store"
)

// newExplorer builds the GitHub client (replaceable in tests).
var newExplorer = explore.NewClient

// catalogs returns every marketplace to search: the ones Claude Code has
// downloaded, then the ones in the config. Unreachable ones are warnings.
func catalogs(ctx context.Context, cfg *config.Config, c *explore.Client, warn io.Writer) []struct {
	m    explore.Marketplace
	home string
} {
	type cat = struct {
		m    explore.Marketplace
		home string
	}
	var out []cat
	home, _ := os.UserHomeDir()
	for _, lc := range agentkit.NewClaude(home).Catalogs() {
		raw, err := os.ReadFile(lc.File)
		if err != nil {
			continue
		}
		var m explore.Marketplace
		if json.Unmarshal(raw, &m) != nil {
			fmt.Fprintf(warn, "agyh: warning: %s: not a marketplace.json\n", shortPath(lc.File))
			continue
		}
		if m.Name == "" {
			m.Name = lc.Name
		}
		out = append(out, cat{m, lc.Source})
	}
	for _, where := range cfg.Catalogs {
		m, h, err := c.FetchCatalog(ctx, where)
		if err != nil {
			fmt.Fprintln(warn, "agyh: warning:", err)
			continue
		}
		out = append(out, cat{m, h})
	}
	return out
}

func skillsSearchCmd() *cobra.Command {
	var (
		asJSON, offline bool
		limit           int
	)
	cmd := &cobra.Command{
		Use:   "search <words...>",
		Short: "Find skills and plugins in marketplace catalogs and on GitHub",
		Long: `Searches plugin marketplace catalogs (the ones Claude Code has downloaded, and
any listed under "catalogs" in the config) and GitHub repositories tagged
agent-skills or claude-skills. Set GITHUB_TOKEN for higher GitHub rate limits.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			c := newExplorer()
			var results []explore.Result
			for _, cat := range catalogs(cmd.Context(), cfg, c, cmd.ErrOrStderr()) {
				results = append(results, explore.SearchCatalog(cat.m, cat.home, args)...)
			}
			if !offline {
				gh, err := c.SearchGitHub(cmd.Context(), args, limit)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "agyh: warning: GitHub search:", err)
				}
				results = append(results, gh...)
			}
			w := cmd.OutOrStdout()
			if asJSON {
				if results == nil {
					results = []explore.Result{}
				}
				return printJSON(w, results)
			}
			if len(results) == 0 {
				fmt.Fprintln(w, "Nothing found.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tFROM\t★\tDESCRIPTION\tINSTALL")
			for _, r := range results {
				stars := ""
				if r.Stars > 0 {
					stars = fmt.Sprint(r.Stars)
				}
				install := r.Install
				if install == "" {
					install = "(not on GitHub: install it with its agent)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Catalog, stars, model.Truncate(r.Description, 60), install)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&offline, "offline", false, "only search catalogs already on this machine")
	cmd.Flags().IntVarP(&limit, "limit", "n", 15, "maximum GitHub results")
	return cmd
}

// resolveInstallRef turns "<plugin>@<catalog>" into its GitHub source.
func resolveInstallRef(ctx context.Context, cfg *config.Config, c *explore.Client, ref string, warn io.Writer) (explore.Ref, error) {
	if name, catalog, ok := strings.Cut(ref, "@"); ok && !strings.HasPrefix(ref, "github:") && !strings.HasPrefix(ref, "https://") && !strings.Contains(name, "/") {
		for _, cat := range catalogs(ctx, cfg, c, warn) {
			if cat.m.Name != catalog {
				continue
			}
			for _, r := range explore.SearchCatalog(cat.m, cat.home, nil) {
				if r.Name == name {
					if r.Source == "" {
						return explore.Ref{}, fmt.Errorf("%s is not on GitHub: install it with its agent (e.g. claude plugin install %s)", ref, ref)
					}
					return explore.ParseRef(r.Source)
				}
			}
			return explore.Ref{}, fmt.Errorf("no plugin %q in catalog %q", name, catalog)
		}
		return explore.Ref{}, fmt.Errorf("unknown catalog %q (see `agyh skills search`)", catalog)
	}
	return explore.ParseRef(ref)
}

func skillsInstallCmd() *cobra.Command {
	var (
		to       []string
		yes, dry bool
	)
	cmd := &cobra.Command{
		Use:   "install <github:owner/repo[/path][@rev] | URL | plugin@catalog> --to <agents>",
		Short: "Install skills from GitHub or a catalog, pinned to a commit, after a preview",
		Long: `Fetches a skill repository (or one folder of it) at an exact commit, shows what
it contains (skills, and any scripts your agents could run), asks for
confirmation and installs it like agyh skills link: tracked, and never over
a folder agyh did not create. Examples:

  agyh skills install github:owner/skills/skills/pdf --to claude,codex
  agyh skills install https://github.com/owner/repo/tree/v1.2/skills/pdf --to claude
  agyh skills install ponytail@some-marketplace --to codex --dry-run

The fetched copy is kept in agyh's data dir, named by its commit.

` + manageAgentsHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if !dry && !cfg.ManageAgents {
				return errors.New(manageAgentsHelp)
			}
			if !dry && len(to) == 0 {
				return errors.New("say where to install with --to (e.g. --to claude,codex)")
			}
			w, ctx := cmd.OutOrStdout(), cmd.Context()
			c := newExplorer()
			ref, err := resolveInstallRef(ctx, cfg, c, args[0], cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			sha, err := c.Resolve(ctx, ref)
			if err != nil {
				return err
			}
			slug := strings.ReplaceAll(ref.Path, "/", "_")
			if slug == "" {
				slug = "_"
			}
			// The last folder keeps its own name: when the ref points straight
			// at a skill, that name becomes the installed folder's name.
			base := ref.Repo
			if ref.Path != "" {
				base = path.Base(ref.Path)
			}
			dir := filepath.Join(store.DataDir(), "sources", "github", ref.Owner, ref.Repo, slug+"@"+sha[:12], base)
			if _, err := os.Stat(dir); err != nil {
				if err := c.Fetch(ctx, ref, sha, dir); err != nil {
					return err
				}
			}
			pinned := fmt.Sprintf("github:%s/%s@%s", ref.Owner, ref.Repo, sha[:12])
			var skills []agentkit.Skill
			for _, d := range explore.SkillDirs(dir) {
				s, err := agentkit.ReadSkillDir(d, "github", "source", pinned)
				if err != nil {
					fmt.Fprintf(w, "! %s: %v\n", filepath.Base(d), err)
					continue
				}
				skills = append(skills, s)
			}
			if len(skills) == 0 {
				return fmt.Errorf("%s has no skill (no SKILL.md found)", ref)
			}

			// Preview: what will run in your agents.
			review := fmt.Sprintf("https://github.com/%s/%s/tree/%s", ref.Owner, ref.Repo, sha)
			if ref.Path != "" {
				review += "/" + ref.Path
			}
			fmt.Fprintf(w, "%s, pinned to commit %s\nReview it: %s\n\n", ref, sha[:12], review)
			for _, s := range skills {
				files, scripts := inventory(s.Path)
				fmt.Fprintf(w, "• %s: %s\n  %d files", s.Name, model.Truncate(s.Description, 100), files)
				if len(scripts) > 0 {
					fmt.Fprintf(w, "; scripts your agent could run: %s", strings.Join(scripts, ", "))
				}
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, "\nSkills are instructions your agents follow, and their scripts run with your permissions. Install only what you trust.")
			if dry {
				fmt.Fprintln(w, "(dry run: nothing was installed)")
				return nil
			}
			if !yes {
				fmt.Fprintf(w, "Install %d skill(s) into %s? [y/N] ", len(skills), strings.Join(to, ", "))
				line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				switch strings.ToLower(strings.TrimSpace(line)) {
				case "y", "yes", "s", "sim":
				default:
					fmt.Fprintln(w, "Cancelled: nothing was installed.")
					return nil
				}
			}
			ledger, err := agentkit.LoadLedger(ledgerPath())
			if err != nil {
				return err
			}
			var steps []agentkit.Step
			for _, s := range skills {
				for _, agent := range to {
					target, err := targetHome(agent)
					if err != nil {
						return err
					}
					st, err := ledger.PlanLink(s, target, pinned)
					if err != nil {
						return fmt.Errorf("%s: %w", s.Name, err)
					}
					steps = append(steps, st)
				}
			}
			return runSteps(w, ledger, steps, false)
		},
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "agents to install into (claude, codex)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "install without asking after the preview")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "fetch and preview only")
	return cmd
}

// inventory counts a skill's files and lists the ones that look runnable.
func inventory(dir string) (int, []string) {
	files, scripts := 0, []string{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files++
		rel, _ := filepath.Rel(dir, p)
		info, _ := d.Info()
		ext := strings.ToLower(filepath.Ext(p))
		runnable := info != nil && info.Mode().Perm()&0o111 != 0
		switch ext {
		case ".sh", ".bash", ".zsh", ".py", ".js", ".mjs", ".ts", ".rb", ".pl", ".ps1", ".bat", ".cmd", ".exe":
			runnable = true
		}
		if runnable && len(scripts) < 8 {
			scripts = append(scripts, filepath.ToSlash(rel))
		}
		return nil
	})
	return files, scripts
}
