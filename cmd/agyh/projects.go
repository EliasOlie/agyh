package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/colors"
	"github.com/EliasOlie/agyh/internal/store"
)

func (a *app) projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"projects", "p"},
		Short:   "Group sessions from any agent into projects",
	}

	var (
		desc  string
		force bool
	)
	newCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := guardSimilar(cmd, st, args[0], force); err != nil {
				return err
			}
			if err := st.CreateProject(cmd.Context(), args[0], desc); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created project %s\n", args[0])
			return nil
		},
	}
	newCmd.Flags().StringVarP(&desc, "description", "d", "", "what the project is about")
	newCmd.Flags().BoolVar(&force, "force", false, "create it even if an existing project has a near-identical name")

	var asJSON bool
	lsCmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List projects",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			ps, err := st.Projects(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				if ps == nil {
					ps = []store.Project{}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(ps)
			}
			if len(ps) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No projects yet: agyh project add <name> <session-id>")
				return nil
			}
			// Colors are applied after alignment: escape codes would skew tabwriter.
			width := len("PROJECT")
			for _, p := range ps {
				width = max(width, len(p.Name))
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "  %-*s\tSESSIONS\tLAST ACTIVE\tDESCRIPTION\n", width, "PROJECT")
			for _, p := range ps {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", paint("● "+fmt.Sprintf("%-*s", width, p.Name), p.Name, p.Color), p.Sessions, bridge.Ago(p.LastActive), p.Description)
			}
			return tw.Flush()
		},
	}
	lsCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var (
		note     string
		addForce bool
	)
	addCmd := &cobra.Command{
		Use:   "add <project> <id...>",
		Short: "Save sessions into a project (created if needed)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()
			if err := guardSimilar(cmd, st, args[0], addForce); err != nil {
				return err
			}
			for _, ref := range args[1:] {
				sess, err := resolveSession(cmd, st, ref)
				if err != nil {
					return err
				}
				if err := st.AddToProject(cmd.Context(), args[0], sess, note); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Saved %s:%s into %s\n", sess.Agent, sess.ShortID(), args[0])
			}
			return nil
		},
	}
	addCmd.Flags().StringVar(&note, "note", "", "note to remember why this session matters")
	addCmd.Flags().BoolVar(&addForce, "force", false, "create the project even if an existing one has a near-identical name")

	rmCmd := &cobra.Command{
		Use:   "rm <project> <id>",
		Short: "Remove a session from a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			sess, err := resolveSession(cmd, st, args[1])
			if err != nil {
				return err
			}
			if err := st.RemoveFromProject(cmd.Context(), args[0], sess); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%s is not in project %s", sess.ShortID(), args[0])
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from %s\n", sess.ShortID(), args[0])
			return nil
		},
	}

	var showFlags listFlags
	showCmd := &cobra.Command{
		Use:   "show <project>",
		Short: "List the sessions of a project",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()
			showFlags.project = args[0]
			hits, err := st.List(cmd.Context(), showFlags.filter())
			if err != nil {
				return err
			}
			return printHits(cmd.OutOrStdout(), hits, showFlags.json, projectColors(cmd.Context(), st))
		},
	}
	showCmd.Flags().IntVarP(&showFlags.limit, "limit", "n", 100, "maximum sessions to show")
	showCmd.Flags().BoolVar(&showFlags.json, "json", false, "print JSON")

	renameCmd := &cobra.Command{
		Use:   "rename <old> <new>",
		Short: "Rename a project",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.RenameProject(cmd.Context(), args[0], args[1]); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no project named %s", args[0])
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed %s to %s\n", args[0], args[1])
			return nil
		},
	}

	deleteCmd := &cobra.Command{
		Use:   "delete <project>",
		Short: "Delete a project; its sessions are kept (and it won't come back from the vault)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.DeleteProject(cmd.Context(), args[0]); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no project named %s", args[0])
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted project %s (its sessions are kept)\n", args[0])
			return nil
		},
	}

	colorCmd := &cobra.Command{
		Use:   "color <project> <color>",
		Short: "Set a project's color (" + paletteNames() + ", #RRGGBB, or auto)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := colors.Parse(args[1])
			if err != nil {
				return err
			}
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.SetProjectColor(cmd.Context(), args[0], c); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no project named %s", args[0])
			} else if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), paint("● "+args[0], args[0], c)+" is now "+map[bool]string{true: "colored automatically", false: c}[c == ""])
			return nil
		},
	}

	mergeCmd := &cobra.Command{
		Use:   "merge <from> <into>",
		Short: "Move every session of a project into another and delete the first (sessions are kept)",
		Long: `Merges duplicates: every session of <from> joins <into> (created if needed),
notes are kept (when both have one, <into>'s wins), sessions waiting to join
<from> join <into> instead, and <from> is deleted. No session is deleted, and
the vault won't bring <from> back.

  agyh project merge conectame conecta-me`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()
			n, err := st.MergeProjects(cmd.Context(), args[0], args[1])
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("no project named %s", args[0])
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Moved %d sessions from %s into %s; %s deleted (its sessions are kept)\n", n, args[0], args[1], args[0])
			return nil
		},
	}

	cmd.AddCommand(newCmd, lsCmd, addCmd, rmCmd, showCmd, renameCmd, deleteCmd, colorCmd, mergeCmd)
	return cmd
}

// guardSimilar refuses a new project whose name looks like an existing one,
// unless forced: that is how "conectame" and "conecta-me" end up side by side.
func guardSimilar(cmd *cobra.Command, st *store.Store, name string, force bool) error {
	if force {
		return nil
	}
	var sim *store.ErrSimilarProject
	if err := st.CheckNewProject(cmd.Context(), name); errors.As(err, &sim) {
		return fmt.Errorf("%w: use %s, or add --force to create %s anyway", sim, sim.Existing, name)
	} else if err != nil {
		return err
	}
	return nil
}
