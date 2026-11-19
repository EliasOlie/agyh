---
name: agyh-send
description: Send files to the user's phone (Telegram/WhatsApp) with `agyh send`. Use when the user asks to send, share or "mandar" a file to their phone, Telegram or WhatsApp, or when they are away from the computer and a file they asked for (PDF, image, report, export, build artifact) is ready.
---

# Send files to the user's phone

`agyh send` delivers files through the agyh bridge to every chat the user
connected (Telegram, WhatsApp).

```bash
agyh send path/to/report.pdf
agyh send out/a.png out/b.png -m "Telas do checkout, versão 2"
```

- Paths may be relative to the current directory.
- `-m` adds a short caption: say what the file is.
- Files inside the workspace or the temp dir go right away. Anything else
  waits for the user's approval on the phone: prefer creating deliverables
  inside the workspace (or `/tmp`).
- Secrets are refused (keys, `.env` files, credentials, `.git`, `.ssh`): do
  not try to work around it, and never copy a secret elsewhere to send it.
- Limits: 50 MB on Telegram, 100 MB on WhatsApp. Compress or split bigger files.

If it fails with "the bridge is not running", tell the user to start it
(`agyh bridge`, or the agyh-bridge service) instead of retrying.

Files the user sends from the phone are saved in agyh's inbox
(`~/.local/share/agyh/inbox/<date>/`); they may paste that path to you.

<!-- Installed by `agyh install-hooks --apply`. Edit freely: agyh then leaves this file alone. -->
