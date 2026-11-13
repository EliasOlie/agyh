package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/sync"
)

func (a *app) syncCmd() *cobra.Command {
	var (
		asJSON     bool
		statusOnly bool
		pushOnly   bool
		pullOnly   bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize sessions and projects with your self-hosted cloud vault (S3 / VPSQL)",
		Long: `Synchronizes your coding-agent sessions (Antigravity, Claude Code, Codex)
and projects with an encrypted S3-compatible cloud vault.

All sessions are compressed and encrypted client-side using AES-256-GCM.
Configure your endpoint, bucket and credentials in ~/.config/agyh/config.json
or via AGYH_SYNC_* environment variables.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()

			engine, err := sync.NewEngine(cfg.Sync, st)
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			ctx := cmd.Context()

			if statusOnly {
				report, err := engine.Status(ctx)
				if err != nil {
					return err
				}
				if asJSON {
					enc := json.NewEncoder(w)
					enc.SetIndent("", "  ")
					return enc.Encode(report)
				}
				return printSyncStatus(w, report)
			}

			var res sync.SyncResult
			switch {
			case pushOnly:
				res, err = engine.Push(ctx)
			case pullOnly:
				res, err = engine.Pull(ctx)
			default:
				res, err = engine.Sync(ctx)
			}
			if err != nil {
				return err
			}

			if asJSON {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}

			action := "Synced with cloud vault"
			if pushOnly {
				action = "Pushed to cloud vault"
			} else if pullOnly {
				action = "Pulled from cloud vault"
			}

			fmt.Fprintf(w, "✓ %s (%s/%s)\n", action, cfg.Sync.Endpoint, cfg.Sync.Bucket)
			fmt.Fprintf(w, "  Uploaded:   %d sessions\n", res.Uploaded)
			fmt.Fprintf(w, "  Downloaded: %d sessions\n", res.Downloaded)
			fmt.Fprintf(w, "  Total:      %d sessions in vault\n", res.Total)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&statusOnly, "status", false, "show sync status without transferring data")
	cmd.Flags().BoolVar(&pushOnly, "push", false, "only upload local sessions to vault")
	cmd.Flags().BoolVar(&pullOnly, "pull", false, "only download remote sessions from vault")

	// Subcommands
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show differences between local store and cloud vault",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()

			engine, err := sync.NewEngine(cfg.Sync, st)
			if err != nil {
				return err
			}
			report, err := engine.Status(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(report)
			}
			return printSyncStatus(cmd.OutOrStdout(), report)
		},
	}
	statusCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	pushCmd := &cobra.Command{
		Use:   "push",
		Short: "Force upload of all local sessions to cloud vault",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()

			engine, err := sync.NewEngine(cfg.Sync, st)
			if err != nil {
				return err
			}
			res, err := engine.Push(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Pushed %d sessions to vault (%d total)\n", res.Uploaded, res.Total)
			return nil
		},
	}
	pushCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	pullCmd := &cobra.Command{
		Use:   "pull",
		Short: "Download any missing or updated sessions from cloud vault",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			st, err := a.openSynced(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer st.Close()

			engine, err := sync.NewEngine(cfg.Sync, st)
			if err != nil {
				return err
			}
			res, err := engine.Pull(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Pulled %d sessions from vault (%d total)\n", res.Downloaded, res.Total)
			return nil
		},
	}
	pullCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	restoreCmd := &cobra.Command{
		Use:   "restore",
		Short: "Reconstruct local database by restoring all sessions from cloud vault",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			st, err := a.open()
			if err != nil {
				return err
			}
			defer st.Close()

			engine, err := sync.NewEngine(cfg.Sync, st)
			if err != nil {
				return err
			}
			res, err := engine.Restore(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Restored %d sessions from cloud vault into local index\n", res.Downloaded)
			return nil
		},
	}
	restoreCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	cmd.AddCommand(statusCmd, pushCmd, pullCmd, restoreCmd)
	return cmd
}

func printSyncStatus(w io.Writer, r sync.StatusReport) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Endpoint:\t%s\n", r.Endpoint)
	fmt.Fprintf(tw, "Bucket:\t%s\n", r.Bucket)
	fmt.Fprintf(tw, "Local sessions:\t%d\n", r.LocalSessions)
	fmt.Fprintf(tw, "Remote vault:\t%d\n", r.RemoteSessions)
	fmt.Fprintf(tw, "To upload:\t%d\n", r.ToUpload)
	fmt.Fprintf(tw, "To download:\t%d\n", r.ToDownload)
	if !r.LastSynced.IsZero() {
		fmt.Fprintf(tw, "Last vault update:\t%s\n", bridge.Ago(r.LastSynced))
	}
	return tw.Flush()
}
