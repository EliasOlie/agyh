package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/store"
)

func (a *app) projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "project",
		Aliases: []string{"projects", "p"},
		Short:   "Group sessions from any agent into projects",
	}

	var desc string
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
			if err := st.CreateProject(cmd.Context(), args[0], desc); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created project %s\n", args[0])
			return nil
		},
	}
	newCmd.Flags().StringVarP(&desc, "description", "d", "", "what the project is about")

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
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PROJECT\tSESSIONS\tLAST ACTIVE\tDESCRIPTION")
			for _, p := range ps {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", p.Name, p.Sessions, bridge.Ago(p.LastActive), p.Description)
			}
			return tw.Flush()
		},
	}
	lsCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var note string
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
			return printHits(cmd.OutOrStdout(), hits, showFlags.json)
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
		Short: "Delete a project (sessions themselves are untouched)",
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
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted project %s\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(newCmd, lsCmd, addCmd, rmCmd, showCmd, renameCmd, deleteCmd)
	return cmd
}
