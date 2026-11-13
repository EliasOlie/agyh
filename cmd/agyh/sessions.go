package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
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
			return printHits(cmd.OutOrStdout(), hits, f.json)
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
			return printHits(cmd.OutOrStdout(), hits, f.json)
		},
	}
	f.bind(cmd)
	return cmd
}

func printHits(w io.Writer, hits []store.Hit, asJSON bool) error {
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
			title += "  #" + strings.Join(h.Projects, " #")
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
	fmt.Fprintf(os.Stderr, "agyh: %s\n", strings.Join(argv, " "))
	return execReplace(bin, argv)
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
