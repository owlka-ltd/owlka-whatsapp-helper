# owlka-whatsapp-helper

A small program that runs on your own computer and links to your WhatsApp account as a
**linked device**, the same mechanism WhatsApp Web and WhatsApp Desktop use. It keeps the
session and your message history in a folder on your computer, and offers what it can do over
a socket that only programs on the same computer, running as you, can reach.

It is a **separate program**. Other software (for example the Owlka desktop app) talks to it
only through the local socket protocol in [PROTOCOL.md](PROTOCOL.md). Nothing is linked or
imported across that boundary in either direction.

This is not an official WhatsApp product. It is not affiliated with, endorsed by or supported
by WhatsApp or Meta. "WhatsApp" is used here only to say what the program connects to.

## What it does

- Links a phone number with an 8-character pairing code (WhatsApp, Linked devices, Link with
  phone number). Several numbers can be linked side by side.
- Stores chats, messages and contact names in a SQLite file on this computer.
- Lists chats, reads and searches messages, finds contacts.
- Sends a text, a reply, an image or a file, to one chat at a time.
- Marks a chat as read.
- Fetches a photo or file from a message to a local file, only when asked.

## The send rules

It is the user's own number that WhatsApp restricts if it behaves like a bulk sender, so the
rules live here, in the one program that can send:

1. **One message, one chat.** There is no bulk or broadcast route. Status updates, broadcast
   lists and channels are refused. The same text to more than 3 chats in an hour is refused.
2. **Paced like a person.** "Typing" shows first, for a time that grows with the length of the
   message. Two sends are never less than 3 seconds apart. At most 8 a minute, 60 an hour and
   250 a day per number. A refused send is reported, never queued.
3. **Only people you already talk to.** A number with no existing chat is refused unless the
   request repeats that exact number in `confirm_new_number`, and at most 3 such first
   messages go out per day.

These guard against accidents. They are not a lock: the source is here and you can change it.

Reading a chat does not tell the other person you have read it. Only "mark read" does.

## What leaves this computer

The helper talks to WhatsApp's servers, as any linked device does, and to nothing else. It
has no telemetry and makes no other network connection. Its local socket listens on
`127.0.0.1` only and every request needs a random token that is written to a file only your
user can read.

The session keys and the message history are stored unencrypted in the folder given by
`--dir`, readable only by your user account. Anyone who can read your files can read them.

## Risks you should know

- Using an unofficial client is against WhatsApp's Terms of Service. WhatsApp can restrict or
  ban a number that uses one. The rules above lower the chance; they do not remove it.
- WhatsApp changes its protocol without notice. When it does, this program can stop working
  until it is updated.

## Build

Needs the Go version named in `go.mod` (1.27.1) or newer. No C compiler is needed.

```sh
go test ./...
go build -trimpath -ldflags "-s -w" -o owlka-whatsapp .
./build.sh        # macOS arm64, macOS x64 and Windows x64 into dist/
```

Run it with `owlka-whatsapp --dir <folder>`. It writes `helper.json` (port and token) into
that folder. See [PROTOCOL.md](PROTOCOL.md).

## Tests

The tests run the whole program against a fake WhatsApp session. No test connects to
WhatsApp, links a number or sends a message.

## Licence

Copyright (C) 2026 Owlka Ltd.

This program is free software: you can redistribute it and/or modify it under the terms of the
GNU General Public License, version 3, as published by the Free Software Foundation. It is
distributed without any warranty. See [LICENSE](LICENSE).

It is GPL-3.0 because it links `go.mau.fi/libsignal`, which is GPL-3.0.

### What it is built from

| Module | Licence |
|---|---|
| `go.mau.fi/whatsmeow` | MPL-2.0 |
| `go.mau.fi/libsignal` | GPL-3.0 |
| `go.mau.fi/util` | MPL-2.0 |
| `modernc.org/sqlite`, `modernc.org/libc`, `modernc.org/mathutil`, `modernc.org/memory` | BSD-3-Clause |
| `google.golang.org/protobuf` | BSD-3-Clause |
| `golang.org/x/crypto`, `net`, `sync`, `sys`, `text`, `exp` | BSD-3-Clause |
| `filippo.io/edwards25519`, `github.com/google/uuid`, `github.com/remyoudompheng/bigfft` | BSD-3-Clause |
| `github.com/coder/websocket` | ISC |
| `github.com/rs/zerolog`, `github.com/beeper/argo-go`, `github.com/vektah/gqlparser`, `github.com/elliotchance/orderedmap`, `github.com/mattn/go-colorable`, `github.com/mattn/go-isatty`, `github.com/dustin/go-humanize`, `github.com/ncruces/go-strftime` | MIT |
| `github.com/petermattis/goid` | Apache-2.0 |

Exact versions are in `go.mod` and `go.sum`. Each module's own licence text is in its source,
which `go mod download` fetches.
