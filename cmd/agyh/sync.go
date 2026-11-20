package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/EliasOlie/agyh/internal/bridge"
	"github.com/EliasOlie/agyh/internal/config"
	"github.com/EliasOlie/agyh/internal/store"
	"github.com/EliasOlie/agyh/internal/sync"
)

// withVault loads the config, opens the index (re-synced first when reindex
// is set) and hands a vault engine to fn. Every sync command goes through it.
func (a *app) withVault(cmd *cobra.Command, reindex bool, fn func(e *sync.Engine, cfg *config.Config) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var st *store.Store
	if reindex {
		st, err = a.openSynced(cmd.Context(), false)
	} else {
		st, err = a.open()
	}
	if err != nil {
		return err
	}
	defer st.Close()
	engine, err := sync.NewEngine(cfg.Sync, st)
	if err != nil {
		return err
	}
	return fn(engine, cfg)
}

// legacyHint is shown while v1 blobs (shared-salt PBKDF2) remain in the vault.
const legacyHint = "! Some sessions still use the legacy vault encryption (shared salt). Run `agyh sync migrate` to re-encrypt them."

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

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
			return a.withVault(cmd, true, func(engine *sync.Engine, cfg *config.Config) error {
				w, ctx := cmd.OutOrStdout(), cmd.Context()
				if statusOnly {
					return runSyncStatus(cmd, engine, asJSON)
				}
				var (
					res    sync.SyncResult
					err    error
					action = "Synced with cloud vault"
				)
				switch {
				case pushOnly:
					action = "Pushed to cloud vault"
					res, err = engine.Push(ctx)
				case pullOnly:
					action = "Pulled from cloud vault"
					res, err = engine.Pull(ctx)
				default:
					res, err = engine.Sync(ctx)
				}
				if err != nil {
					printKit(cmd.ErrOrStderr(), res.Kit)
					return err
				}
				if asJSON {
					return printJSON(w, res)
				}
				fmt.Fprintf(w, "✓ %s (%s/%s)\n", action, cfg.Sync.Endpoint, cfg.Sync.Bucket)
				fmt.Fprintf(w, "  Uploaded:   %d sessions\n", res.Uploaded)
				fmt.Fprintf(w, "  Downloaded: %d sessions\n", res.Downloaded)
				fmt.Fprintf(w, "  Total:      %d sessions in vault\n", res.Total)
				printKit(w, res.Kit)
				printWarnings(cmd.ErrOrStderr(), res.Warnings)
				if res.LegacyBlobs {
					fmt.Fprintln(w, legacyHint)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&statusOnly, "status", false, "show sync status without transferring data")
	cmd.Flags().BoolVar(&pushOnly, "push", false, "only upload local sessions to vault")
	cmd.Flags().BoolVar(&pullOnly, "pull", false, "only download remote sessions from vault")

	cmd.AddCommand(
		a.syncSub("status", "Show differences between local store and cloud vault", true,
			func(cmd *cobra.Command, e *sync.Engine, asJSON bool) error { return runSyncStatus(cmd, e, asJSON) }),
		a.syncSub("push", "Force upload of all local sessions to cloud vault", true,
			transfer((*sync.Engine).Push, func(r sync.SyncResult) string {
				return fmt.Sprintf("✓ Pushed %d sessions to vault (%d total)", r.Uploaded, r.Total)
			})),
		a.syncSub("pull", "Download any missing or updated sessions from cloud vault", true,
			transfer((*sync.Engine).Pull, func(r sync.SyncResult) string {
				return fmt.Sprintf("✓ Pulled %d sessions from vault (%d total)", r.Downloaded, r.Total)
			})),
		a.syncSub("migrate", "Re-encrypt sessions still using the legacy vault key (v1) with the vault's own key", false,
			transfer((*sync.Engine).Migrate, func(r sync.SyncResult) string {
				return fmt.Sprintf("✓ Re-encrypted %d sessions with this vault's own salt (Argon2id); %d in vault", r.Uploaded, r.Total)
			})),
		a.restoreCmd(),
	)
	return cmd
}

// restoreCmd rebuilds the index from the vault and restores this machine's
// kit (skills, plugin list, MCP servers) into agyh's data dir.
func (a *app) restoreCmd() *cobra.Command {
	var (
		asJSON  bool
		machine string
	)
	c := &cobra.Command{
		Use:   "restore",
		Short: "Rebuild the local index from the vault, and restore this machine's skills, plugins and MCP servers",
		Long: `Downloads every session from the vault into the local index, then restores a
machine's kit (skills, plugin list, MCP servers with their secrets) into agyh's
own data directory. Agents' configurations are never modified.

By default the kit of the machine with this name (sync.machine, else the
hostname) is restored; after reinstalling a machine under a new name, pick the
old one with --machine (agyh sync status lists them).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// restore rebuilds the index from the vault, so it must not index first.
			return a.withVault(cmd, false, func(e *sync.Engine, cfg *config.Config) error {
				w, ctx := cmd.OutOrStdout(), cmd.Context()
				res, err := e.Restore(ctx)
				if err != nil {
					return err
				}
				out := struct {
					sync.SyncResult
					Kit      *sync.KitResult   `json:"kit,omitempty"`
					Machines []sync.KitMachine `json:"kit_machines,omitempty"`
				}{SyncResult: res}
				var kitErr error
				if cfg.Sync.KitEnabled() {
					kit, err := e.RestoreKit(ctx, machine)
					switch {
					case errors.Is(err, sync.ErrNoKit):
						out.Machines, _ = e.KitMachines(ctx)
						kitErr = fmt.Errorf("%w; machines in the vault: %s (choose one with --machine)", err, machineNames(out.Machines))
					case err != nil:
						kitErr = fmt.Errorf("sessions restored, but the kit restore failed: %w", err)
					default:
						out.Kit = &kit
					}
				}
				if asJSON {
					if err := printJSON(w, out); err != nil {
						return err
					}
					return kitErr
				}
				fmt.Fprintf(w, "✓ Restored %d sessions from cloud vault into local index\n", res.Downloaded)
				if out.Kit != nil {
					k := out.Kit
					extra := ""
					if k.ProjectSkills > 0 {
						extra = fmt.Sprintf(" (%d inside projects)", k.ProjectSkills)
					}
					if k.Files > 0 {
						extra += fmt.Sprintf(", %d agent files", k.Files)
					}
					fmt.Fprintf(w, "✓ Restored the kit of %s: %d skills%s, %d MCP servers, %d plugins\n  into %s\n",
						k.Machine, k.Skills, extra, k.MCP, k.Plugins, shortPath(k.Dir))
					fmt.Fprintln(w, "  Your agents were not modified: their files are untouched. To put things back:")
					m := out.Kit.Machine
					fmt.Fprintf(w, "    agyh skills link --all --from kit:%s   # global skills\n", m)
					fmt.Fprintf(w, "    agyh kit apply --machine %s            # instructions, memories, subagents, project skills\n", m)
					fmt.Fprintf(w, "    agyh mcp apply --all --from kit:%s     # MCP servers\n", m)
					fmt.Fprintln(w, "    agyh resume <id>                         # reopens a session from the vault in its agent")
				}
				return kitErr
			})
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	c.Flags().StringVar(&machine, "machine", "", "restore the kit of this machine (default: this one)")
	return c
}

func machineNames(ms []sync.KitMachine) string {
	if len(ms) == 0 {
		return "none"
	}
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}

// printKit summarizes a kit backup.
func printKit(w io.Writer, k *sync.KitResult) {
	if k == nil {
		return
	}
	extra := ""
	if k.ProjectSkills > 0 {
		extra += fmt.Sprintf(", %d inside projects", k.ProjectSkills)
	}
	files := ""
	if k.Files > 0 {
		files = fmt.Sprintf(", %d agent files", k.Files)
	}
	fmt.Fprintf(w, "  Kit:        %d skills (%d new%s)%s, %d MCP servers, %d plugins, as %q\n", k.Skills, k.NewSkills, extra, files, k.MCP, k.Plugins, k.Machine)
	for _, warn := range k.Warnings {
		fmt.Fprintln(w, "  ! "+warn)
	}
}

// syncSub builds one `agyh sync <name>` subcommand with its own --json flag.
func (a *app) syncSub(name, short string, reindex bool, run func(cmd *cobra.Command, e *sync.Engine, asJSON bool) error) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withVault(cmd, reindex, func(e *sync.Engine, _ *config.Config) error { return run(cmd, e, asJSON) })
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

// transfer adapts an engine operation to a subcommand: JSON or a one-line summary.
func transfer(op func(*sync.Engine, context.Context) (sync.SyncResult, error), summary func(sync.SyncResult) string) func(*cobra.Command, *sync.Engine, bool) error {
	return func(cmd *cobra.Command, e *sync.Engine, asJSON bool) error {
		res, err := op(e, cmd.Context())
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(cmd.OutOrStdout(), res)
		}
		fmt.Fprintln(cmd.OutOrStdout(), summary(res))
		printKit(cmd.OutOrStdout(), res.Kit)
		printWarnings(cmd.ErrOrStderr(), res.Warnings)
		if res.LegacyBlobs {
			fmt.Fprintln(cmd.OutOrStdout(), legacyHint)
		}
		return nil
	}
}

func printWarnings(w io.Writer, warnings []string) {
	for _, s := range warnings {
		fmt.Fprintln(w, "! "+s)
	}
}

func runSyncStatus(cmd *cobra.Command, e *sync.Engine, asJSON bool) error {
	report, err := e.Status(cmd.Context())
	if err != nil {
		return err
	}
	if asJSON {
		return printJSON(cmd.OutOrStdout(), report)
	}
	return printSyncStatus(cmd.OutOrStdout(), report)
}

func printSyncStatus(w io.Writer, r sync.StatusReport) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Endpoint:\t%s\n", r.Endpoint)
	fmt.Fprintf(tw, "Bucket:\t%s\n", r.Bucket)
	fmt.Fprintf(tw, "Local sessions:\t%d\n", r.LocalSessions)
	fmt.Fprintf(tw, "Remote vault:\t%d\n", r.RemoteSessions)
	fmt.Fprintf(tw, "To upload:\t%d\n", r.ToUpload)
	fmt.Fprintf(tw, "To download:\t%d\n", r.ToDownload)
	enc := "v2: Argon2id, per-vault salt"
	if r.LegacyEncryption {
		enc = "legacy v1 blobs present: run `agyh sync migrate`"
	}
	fmt.Fprintf(tw, "Encryption:\t%s\n", enc)
	if !r.LastSynced.IsZero() {
		fmt.Fprintf(tw, "Last vault update:\t%s\n", bridge.Ago(r.LastSynced))
	}
	if r.TranscriptsPossible > 0 {
		note := "resumable in their agent on any machine"
		if r.Transcripts < r.TranscriptsPossible {
			note = "the rest: readable, not resumable (agyh sync adds those this machine has)"
		}
		fmt.Fprintf(tw, "Transcripts:\t%d of %d sessions (%s)\n", r.Transcripts, r.TranscriptsPossible, note)
	}
	fmt.Fprintf(tw, "This machine:\t%s\n", r.Machine)
	if len(r.KitMachines) == 0 {
		fmt.Fprintf(tw, "Kit snapshots:\tnone yet (agyh sync creates one)\n")
	}
	for i, m := range r.KitMachines {
		label := ""
		if i == 0 {
			label = "Kit snapshots:"
		}
		files := ""
		if m.Files > 0 {
			files = fmt.Sprintf(", %d agent files", m.Files)
		}
		fmt.Fprintf(tw, "%s\t%s: %d skills%s, %d MCP servers, %d plugins, %s\n", label, m.Name, m.Skills, files, m.MCP, m.Plugins, bridge.Ago(m.UpdatedAt))
	}
	return tw.Flush()
}
