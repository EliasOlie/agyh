#!/bin/sh
# agyh installer: downloads a release binary, checks it against the release's
# checksums, installs it and wires agyh into the coding agents it finds.
#
#   curl -fsSL https://raw.githubusercontent.com/EliasOlie/agyh/main/install.sh | sh
#   curl -fsSL …/install.sh | sh -s -- --version v0.4.0 --service
#
# Options (or environment variables):
#   --version vX.Y.Z   AGYH_VERSION   release to install (default: latest)
#   --bin-dir DIR      AGYH_BIN_DIR   where to put agyh (default: ~/.local/bin)
#   --no-hooks         AGYH_NO_HOOKS=1  don't touch the agents' settings
#   --service          AGYH_SERVICE=1   run the bridge as a user service
#                                       (systemd on Linux, launchd on macOS)
#
# Hooks are written with `agyh install-hooks --apply`: only agyh's own hooks
# are replaced, broken ones (pointing at a deleted program) are removed, and
# each settings file is backed up as *.agyh-backup first.
set -eu

REPO="EliasOlie/agyh"
VERSION="${AGYH_VERSION:-latest}"
BIN_DIR="${AGYH_BIN_DIR:-$HOME/.local/bin}"
HOOKS=1
[ "${AGYH_NO_HOOKS:-}" = 1 ] && HOOKS=0
SERVICE="${AGYH_SERVICE:-0}"

while [ $# -gt 0 ]; do
	case "$1" in
	--version) VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--bin-dir) BIN_DIR="$2"; shift 2 ;;
	--bin-dir=*) BIN_DIR="${1#*=}"; shift ;;
	--no-hooks) HOOKS=0; shift ;;
	--service) SERVICE=1; shift ;;
	-h | --help) sed -n '2,19p' "$0" 2>/dev/null || true; exit 0 ;;
	*) echo "agyh install: unknown option $1" >&2; exit 2 ;;
	esac
done

say() { printf '%s\n' "$*"; }
die() { printf 'agyh install: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "needs $1"; }

need curl
need tar
need uname

case "$(uname -s)" in
Linux) OS=linux ;;
Darwin) OS=darwin ;;
*) die "unsupported system $(uname -s). On Windows download the zip from https://github.com/$REPO/releases, or: go install github.com/$REPO/cmd/agyh@latest" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*) die "unsupported CPU $(uname -m)" ;;
esac

if [ "$VERSION" = latest ]; then
	# The /releases/latest page redirects to /releases/tag/<tag>: no API token or rate limit.
	url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest") ||
		die "could not reach GitHub"
	VERSION="${url##*/}"
fi
case "$VERSION" in
v[0-9]*) ;;
*) die "bad version '$VERSION' (expected something like v0.4.0)" ;;
esac

ARCHIVE="agyh_${VERSION}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "→ Downloading agyh $VERSION ($OS/$ARCH)"
curl -fsSL -o "$TMP/$ARCHIVE" "$BASE/$ARCHIVE" || die "download failed: $BASE/$ARCHIVE"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt" || die "download failed: checksums.txt"

want=$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "$TMP/checksums.txt")
[ -n "$want" ] || die "$ARCHIVE is not listed in checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$TMP/$ARCHIVE" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$TMP/$ARCHIVE" | awk '{ print $1 }')
else
	die "needs sha256sum or shasum to verify the download"
fi
[ "$want" = "$got" ] || die "checksum mismatch for $ARCHIVE: refusing to install"
say "✓ Checksum verified"
if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
	if gh attestation verify "$TMP/$ARCHIVE" --repo "$REPO" >/dev/null 2>&1; then
		say "✓ Build provenance verified (GitHub attestation)"
	else
		die "GitHub attestation check failed for $ARCHIVE: refusing to install"
	fi
fi

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" agyh
mkdir -p "$BIN_DIR"
UPGRADE=0
[ -x "$BIN_DIR/agyh" ] && UPGRADE=1
# Replace atomically: a bridge or hook may be running the old binary.
cp "$TMP/agyh" "$BIN_DIR/.agyh.new"
chmod 0755 "$BIN_DIR/.agyh.new"
mv -f "$BIN_DIR/.agyh.new" "$BIN_DIR/agyh"
AGYH="$BIN_DIR/agyh"
say "✓ Installed $("$AGYH" --version) at $AGYH"

case ":$PATH:" in
*":$BIN_DIR:"*) ;;
*) say "! $BIN_DIR is not on your PATH. Add it, e.g.: echo 'export PATH=\"\$PATH:$BIN_DIR\"' >> ~/.profile" ;;
esac

if [ "$HOOKS" = 1 ]; then
	say "→ Wiring agyh into your agents"
	"$AGYH" install-hooks --apply || say "! No agent hooks installed (run 'agyh install-hooks' later)"
fi

if [ "$SERVICE" = 1 ]; then
	case "$OS" in
	linux)
		command -v systemctl >/dev/null 2>&1 || die "--service needs systemd (systemctl --user)"
		unit="$HOME/.config/systemd/user/agyh-bridge.service"
		mkdir -p "$(dirname "$unit")"
		cat >"$unit" <<EOF
[Unit]
Description=agyh bridge (agent approvals, chat commands and files over Telegram/WhatsApp)
After=network-online.target

[Service]
ExecStart=$AGYH bridge
Environment=PATH=$BIN_DIR:/usr/local/bin:/usr/bin:/bin
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
		systemctl --user daemon-reload
		systemctl --user enable agyh-bridge >/dev/null 2>&1
		systemctl --user restart agyh-bridge
		say "✓ Bridge running as a systemd user service (journalctl --user -u agyh-bridge -f)"
		;;
	darwin)
		plist="$HOME/Library/LaunchAgents/io.github.eliasolie.agyh-bridge.plist"
		mkdir -p "$(dirname "$plist")" "$HOME/Library/Logs"
		cat >"$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>io.github.eliasolie.agyh-bridge</string>
	<key>ProgramArguments</key><array><string>$AGYH</string><string>bridge</string></array>
	<key>EnvironmentVariables</key><dict><key>PATH</key><string>$BIN_DIR:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string></dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardErrorPath</key><string>$HOME/Library/Logs/agyh-bridge.log</string>
</dict>
</plist>
EOF
		launchctl unload "$plist" >/dev/null 2>&1 || true
		launchctl load "$plist"
		say "✓ Bridge running as a launchd agent (logs: ~/Library/Logs/agyh-bridge.log)"
		;;
	esac
elif [ "$UPGRADE" = 1 ] && [ "$OS" = linux ] && command -v systemctl >/dev/null 2>&1 &&
	systemctl --user is-active --quiet agyh-bridge 2>/dev/null; then
	systemctl --user restart agyh-bridge && say "✓ Restarted the running bridge service"
elif [ "$UPGRADE" = 1 ] && [ "$OS" = darwin ] &&
	launchctl list io.github.eliasolie.agyh-bridge >/dev/null 2>&1; then
	launchctl kickstart -k "gui/$(id -u)/io.github.eliasolie.agyh-bridge" && say "✓ Restarted the running bridge agent"
fi

say ""
say "Next steps:"
say "  agyh connect telegram     # or: agyh connect whatsapp"
[ "$SERVICE" = 1 ] || say "  agyh bridge               # or re-run this installer with --service"
say "  agyh                      # browse every session"
