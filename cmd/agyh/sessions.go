package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/index"
	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/source"
	"github.com/EliasOlie/agyh/internal/store"
)

type listFlags struct {
	limit     int
	agent     string
	project   string
	workspace string
	all       bool
	json      bool
}

func (f *listFlags) bind(cmd *cobra.Command) {
	cmd.Flags().IntVarP(&f.limit, "limit", "n", 30, "maximum sessions to show")
	cmd.Flags().StringVarP(&f.agent, "agent", "a", "", "only this agent (agy, claude, codex)")
	cmd.Flags().StringVarP(&f.project, "project", "p", "", "only sessions saved in this project")
	cmd.Flags().StringVarP(&f.workspace, "workspace", "w", "", "only sessions whose workspace contains this text")
	cmd.Flags().BoolVar(&f.all, "all", false, "include sub-agent sessions")
	cmd.Flags().BoolVar(&f.json, "json", false, "print JSON")
}

func (f *listFlags) filter() store.Filter {
	return store.Filter{Agent: f.agent, Project: f.project, Workspace: f.workspace,
		IncludeSubs: f.all || f.project != "", Limit: f.limit}
}

func (a *app) lsCmd() *cobra.Command {
	var f listFlags
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sessions, most recent first",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()
			hits, err := st.List(cmd.Context(), f.filter())
			if err != nil {
				return err
			}
			return printHits(cmd.OutOrStdout(), hits, f.json, projectColors(cmd.Context(), st))
		},
	}
	f.bind(cmd)
	return cmd
}

func (a *app) searchCmd() *cobra.Command {
	var f listFlags
	cmd := &cobra.Command{
		Use:   "search <words...>",
		Short: "Full-text search across every agent (accents and case ignored, prefix match)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()
			hits, err := st.Search(cmd.Context(), joinArgs(args), f.filter())
			if err != nil {
				return err
			}
			return printHits(cmd.OutOrStdout(), hits, f.json, projectColors(cmd.Context(), st))
		},
	}
	f.bind(cmd)
	return cmd
}

func printHits(w io.Writer, hits []store.Hit, asJSON bool, pcolors map[string]string) error {
	if asJSON {
		if hits == nil {
			hits = []store.Hit{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(hits)
	}
	if len(hits) == 0 {
		fmt.Fprintln(w, "No sessions found.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tAGENT\tUPDATED\tTITLE\tWORKSPACE")
	for _, h := range hits {
		title := model.Truncate(h.Title, 60)
		if len(h.Projects) > 0 {
			title += "  " + projectTags(h.Projects, pcolors)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.ShortID(), h.Agent, bridge.Ago(h.UpdatedAt), title, hook.ShortenHome(h.Workspace))
		if h.Snippet != "" {
			fmt.Fprintf(tw, "\t\t\t  %s\t\n", model.Truncate(h.Snippet, 90))
		}
	}
	return tw.Flush()
}

func (a *app) showCmd() *cobra.Command {
	var (
		asJSON bool
		last   int
	)
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a session and its messages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()
			sess, err := resolveSession(cmd, st, args[0])
			if err != nil {
				return err
			}
			src := source.ByName(sess.Agent)
			_, msgs, err := source.LoadSessionOrArchive(cmd.Context(), sess)
			if err != nil {
				return err
			}
			if last > 0 && len(msgs) > last {
				msgs = msgs[len(msgs)-last:]
			}
			w := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"session": sess, "messages": msgs})
			}
			var dir string
			var argv []string
			if src != nil {
				dir, argv = src.Resume(sess)
			}
			fmt.Fprintf(w, "%s\n%s · %s · %s · %d messages\nid: %s",
				sess.Title, sess.Agent, hook.ShortenHome(sess.Workspace), sess.UpdatedAt.Local().Format("2006-01-02 15:04"),
				sess.Messages, sess.ID)
			if dir != "" && len(argv) > 0 {
				fmt.Fprintf(w, "\nresume: cd %s && %s", hook.ShortenHome(dir), strings.Join(argv, " "))
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w)
			for _, m := range msgs {
				who := "› you"
				if m.Role == "assistant" {
					who = "◆ " + sess.Agent
				}
				fmt.Fprintf(w, "%s  %s\n%s\n\n", who, m.At.Local().Format("15:04"), m.Text)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().IntVar(&last, "last", 0, "only the last N messages")
	return cmd
}

func (a *app) resumeCmd() *cobra.Command {
	var (
		latest bool
		agent  string
		dry    bool
	)
	cmd := &cobra.Command{
		Use:   "resume [id]",
		Short: "Reopen a session in its agent and workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			var sess model.Session
			switch {
			case len(args) == 1:
				sess, err = resolveSession(cmd, st, args[0])
			case latest:
				sess, err = st.Latest(cmd.Context(), agent)
			default:
				err = errors.New("give a session id, or --last for the most recent one")
			}
			st.Close()
			if err != nil {
				return err
			}
			if dry {
				dir, argv := source.ByName(sess.Agent).Resume(sess)
				fmt.Fprintf(cmd.OutOrStdout(), "cd %s && %s\n", dir, strings.Join(argv, " "))
				return nil
			}
			return resume(sess)
		},
	}
	cmd.Flags().BoolVarP(&latest, "last", "c", false, "resume the most recent session")
	cmd.Flags().StringVarP(&agent, "agent", "a", "", "with --last: only this agent")
	cmd.Flags().BoolVar(&dry, "print", false, "print the command instead of running it")
	return cmd
}

func resolveSession(cmd *cobra.Command, st *store.Store, ref string) (model.Session, error) {
	sess, err := st.Resolve(cmd.Context(), ref)
	var amb *store.ErrAmbiguous
	switch {
	case errors.Is(err, store.ErrNotFound):
		return sess, fmt.Errorf("no session matches %q (try `agyh search`)", ref)
	case errors.As(err, &amb):
		var sb strings.Builder
		fmt.Fprintf(&sb, "%q matches several sessions:\n", ref)
		for _, m := range amb.Matches {
			fmt.Fprintf(&sb, "  %s:%s  %s\n", m.Agent, m.ID, model.Truncate(m.Title, 60))
		}
		return sess, errors.New(strings.TrimRight(sb.String(), "\n"))
	}
	return sess, err
}

// resume replaces this process with the agent, inside the session's workspace.
func resume(sess model.Session) error {
	src := source.ByName(sess.Agent)
	if src == nil {
		return fmt.Errorf("unknown agent %q", sess.Agent)
	}
	dir, argv := src.Resume(sess)
	return launch(dir, argv)
}

// launch replaces this process with argv, inside dir when it still exists.
func launch(dir string, argv []string) error {
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%s is not installed or not in PATH", argv[0])
	}
	if dir != "" {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			if err := os.Chdir(dir); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(os.Stderr, "agyh: workspace %s no longer exists, staying in the current directory\n", dir)
		}
	}
	fmt.Fprintf(os.Stderr, "agyh: %s\n", model.Truncate(strings.Join(argv, " "), 200))
	return execFn(bin, argv)
}

// execFn replaces this process with the agent (replaceable in tests).
var execFn = execReplace

// NewSession is a session to start: agent, workspace, optional first
// prompt, and the project it should join once it shows up in the index.
type NewSession struct {
	Agent, Dir, Prompt, Project string
}

// startSession records the pending project (if any) and starts the agent.
func (a *app) startSession(ctx context.Context, n NewSession) error {
	src := source.ByName(n.Agent)
	if src == nil {
		return fmt.Errorf("unknown agent %q (claude, agy, codex)", n.Agent)
	}
	argv := src.Start(n.Prompt)
	if _, err := exec.LookPath(argv[0]); err != nil {
		return fmt.Errorf("%s is not installed or not in PATH", argv[0])
	}
	// Agents record the directory they run in with symlinks resolved (on
	// macOS /var is /private/var): use the same form, or a pending project
	// would never match the session.
	if dir, err := filepath.EvalSymlinks(n.Dir); err == nil {
		n.Dir = dir
	}
	if n.Project != "" {
		st, err := a.open()
		if err != nil {
			return err
		}
		var sim *store.ErrSimilarProject
		if err := st.CheckNewProject(ctx, n.Project); errors.As(err, &sim) {
			st.Close()
			return fmt.Errorf("%w: use -p %s (or create %s first with agyh project new --force)", sim, sim.Existing, n.Project)
		}
		err = st.AddPendingProject(ctx, n.Agent, n.Dir, n.Project, time.Now())
		st.Close()
		if err != nil {
			return err
		}
	}
	return launch(n.Dir, argv)
}

// resolveWorkspace accepts "@name" (a configured workspace), a path, or "" (the current directory).
func resolveWorkspace(ws string) (string, error) {
	if ws == "" {
		return os.Getwd()
	}
	if name, ok := strings.CutPrefix(ws, "@"); ok {
		cfg, err := config.Load()
		if err != nil {
			return "", err
		}
		for _, w := range cfg.Workspaces {
			if filepath.Base(w) == name || w == name {
				return w, nil
			}
		}
		return "", fmt.Errorf("no configured workspace named %q (see \"workspaces\" in the config)", name)
	}
	if strings.HasPrefix(ws, "~/") {
		home, _ := os.UserHomeDir()
		ws = filepath.Join(home, ws[2:])
	}
	abs, err := filepath.Abs(ws)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

func (a *app) newCmd() *cobra.Command {
	var workspace, project string
	cmd := &cobra.Command{
		Use:   "new [claude|agy|codex] [prompt...]",
		Short: "Start a new session of an agent in a workspace, optionally inside a project",
		Long: `Starts a new interactive session, like running the agent yourself, in a
workspace (default: the current directory). With a prompt, the agent answers
it and stays open. With --project, the session joins that project as soon as
agyh sees it (the next time agyh runs).

  agyh new                                  # claude in the current directory
  agyh new agy -w ~/work/api "fix the failing test"
  agyh new claude -w @pdv -p pagamentos     # a configured workspace, in a project

Codex support follows OpenAI's public docs and has not been tested against a
real Codex install.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			agent := "claude"
			if len(args) > 0 && source.ByName(args[0]) != nil {
				agent, args = args[0], args[1:]
			}
			dir, err := resolveWorkspace(workspace)
			if err != nil {
				return err
			}
			return a.startSession(cmd.Context(), NewSession{Agent: agent, Dir: dir, Prompt: joinArgs(args), Project: project})
		},
	}
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "", "directory to start in, or @name of a configured workspace (default: current directory)")
	cmd.Flags().StringVarP(&project, "project", "p", "", "project the new session joins")
	return cmd
}

func (a *app) indexCmd() *cobra.Command {
	var (
		full   bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Update the search index (only changed sessions are re-read)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			stats, err := index.Sync(cmd.Context(), st, source.All(), full)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(stats)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tFOUND\tSESSIONS\tUPDATED\tREMOVED\tERRORS")
			for _, s := range stats {
				found := "yes"
				if !s.Available {
					found = "no"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\n", s.Agent, found, s.Seen, s.Updated, s.Removed, s.Errors)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "re-read every session")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func joinArgs(args []string) string { return strings.TrimSpace(strings.Join(args, " ")) }
