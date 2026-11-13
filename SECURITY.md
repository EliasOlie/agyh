# Security Policy

agyh can approve shell commands on your machine, so security reports get priority.

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

**Please do not open a public issue.** Use GitHub's private vulnerability reporting:
[Report a vulnerability](https://github.com/EliasOlie/agyh/security/advisories/new).

Include, if you can:

- the agyh version (`agyh --version`) and OS;
- which component is affected (hook, bridge, Telegram/WhatsApp channel, policy, sync/vault, TUI);
- steps to reproduce or a proof of concept;
- the impact you expect (e.g. approval bypass, command injection, secret disclosure).

What to expect:

- acknowledgement within **3 business days**;
- an initial assessment within **7 days**;
- a fix and a GitHub Security Advisory crediting you (unless you prefer otherwise) once a patched release is out.

## Scope

In scope: anything that lets a command get approved without the user, runs chat input in a shell,
exposes the bridge beyond loopback, leaks the bridge secret, bot token, WhatsApp session or vault
passphrase, or weakens the vault encryption.

Out of scope: issues that require an attacker who already runs code as your user, and WhatsApp
account restrictions caused by using an unofficial client (documented in the README).
