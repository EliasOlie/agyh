# Migrating to agyh v0.3.0

v0.3.0 changes how the **cloud vault** is encrypted. If you never configured `"sync"` in `~/.config/agyh/config.json`, there is nothing to migrate: upgrade and carry on.

## What changed and why

| | agyh ≤ 0.2 (vault v1) | agyh ≥ 0.3 (vault v2) |
|---|---|---|
| Key derivation | PBKDF2-SHA256, 100,000 rounds | Argon2id, t=3, 64 MiB, p=4 |
| Salt | **one constant salt shared by every agyh user** | 16 random bytes, unique to each vault |
| Wrong passphrase / altered header | detected while decrypting | detected by a key check **before anything is written** |
| Blob magic | `AGY1` | `AGY2` |

With a shared salt, one precomputed table of passphrase guesses works against every leaked agyh bucket at once. A per-vault salt makes each vault a separate, much more expensive target. The salt and KDF settings live in a new plaintext object, `<prefix>/vault.json`. None of it is secret, and the key is needed to read anything else.

**Your passphrase does not change.** Neither does the config file.

## ⚠️ Compatibility

- **agyh ≥ 0.3 reads both formats.** v1 and v2 blobs can live in the same vault, so nothing becomes unreadable for upgraded machines at any point.
- **agyh ≤ 0.2 cannot read a vault once it has a v2 header.** It stops with
  `agyh: decrypt manifest: decompress gzip: gzip: invalid header`
  and **writes nothing**: your data is safe, but that machine can't sync until it is upgraded.

So: **upgrade every machine that shares the vault before (or right after) the first sync with v0.3.**

## Steps

### 0. Back up the bucket (recommended)

The migration rewrites objects in place. A copy costs nothing and makes it fully reversible:

```bash
# any S3 tool works; with rclone, for example:
rclone sync myremote:agyh-vault myremote:agyh-vault-backup-v1
```

### 1. Upgrade agyh on every machine

```bash
go install github.com/EliasOlie/agyh/cmd/agyh@v0.3.0
agyh --version   # must print v0.3.0 or later
```

**If you run the bridge as a service, restart it.** A running daemon keeps the old binary in memory, and `/sync` from chat would use it:

```bash
systemctl --user restart agyh-bridge
```

### 2. Check the vault (read-only)

```bash
agyh sync status
```

You should see `Encryption: legacy v1 blobs present: run agyh sync migrate`. Read-only commands (`status`, `pull`, `restore`) never change the vault.

### 3. Sync once, from one machine

```bash
agyh sync
```

This creates `vault.json` with the vault's random salt. From now on every blob written by any upgraded machine is v2. The output ends with a reminder while old blobs remain.

Run this step on **one machine first**. Two machines creating the header at the same moment is handled (conditional write, then everyone adopts the stored header), but some self-hosted S3 servers ignore conditional writes, so one machine first is the safe order.

### 4. Re-encrypt the old sessions

```bash
agyh sync migrate
# ✓ Re-encrypted 120 sessions with this vault's own salt (Argon2id); 135 in vault
```

`migrate` rewrites every remaining v1 blob and the manifest, then clears the legacy flag. You can interrupt it and run it again: it only touches what is still v1, and a run with nothing left to do is a no-op.

### 5. Confirm

```bash
agyh sync status
# Encryption:  v2: Argon2id, per-vault salt
```

Then run `agyh sync` on the other machines as usual.

## Troubleshooting

| Message | Meaning | What to do |
|---|---|---|
| `decrypt manifest: decompress gzip: gzip: invalid header` | This machine still runs agyh ≤ 0.2 against a v2 vault. | Upgrade it (step 1). Nothing was written. |
| `invalid passphrase or corrupted vault data (or the vault header was modified)` | Wrong passphrase, or `vault.json` was changed. | Check `"passphrase"` / `AGYH_SYNC_PASSPHRASE`. If it is right, someone altered the header: restore `vault.json` from your backup. Nothing was written. |
| `invalid vault header: kdf parameters below the minimum …` | `vault.json` asks for a weaker KDF than agyh accepts. | Someone tampered with the header, so treat it as an incident and restore `vault.json` from backup. agyh refuses to write with weakened parameters. |
| `invalid vault header: unsupported format N (upgrade agyh)` | The vault was created by a newer agyh. | Upgrade this machine. |
| `migrate` stopped halfway (network, Ctrl+C) | Some blobs are v2, some still v1. Both are readable. | Run `agyh sync migrate` again. |

## Rolling back

v2 vaults can't be read by agyh ≤ 0.2, so rolling back means going back to the backup from step 0:

1. Reinstall the old version on every machine: `go install github.com/EliasOlie/agyh/cmd/agyh@v0.2.1`.
2. Restore the bucket from the backup (for example `rclone sync myremote:agyh-vault-backup-v1 myremote:agyh-vault`), which also removes `vault.json`.

Sessions synced after the backup are not in it. Each machine still has them in its local index and agent files, and the old `agyh sync` uploads them again.

## Unencrypted vaults

If `"passphrase"` is empty, blobs were never encrypted, and v0.3 keeps them that way: `vault.json` is still created, but there is no key and nothing to re-encrypt. `agyh sync status` keeps showing the legacy notice until you run `agyh sync migrate` once, which rewrites nothing and only clears the flag. Setting a passphrase on an existing unencrypted vault is not a migration path; start a new vault (new `prefix` or bucket) and run `agyh sync push`.
