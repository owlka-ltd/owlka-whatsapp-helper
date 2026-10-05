# The local socket protocol

Version 0.1.0. This is the only interface to the helper.

## Finding the helper

Start it with `owlka-whatsapp --dir <folder>`. It listens on `127.0.0.1` on a port the system
picks, and writes `<folder>/helper.json`, readable only by the user:

```json
{"port": 51234, "token": "<64 hex characters>", "pid": 4242, "version": "0.1.0"}
```

Starting a second helper on the same folder exits at once with status 0 and leaves the first
one running.

## Requests

Plain HTTP on `http://127.0.0.1:<port>`. Every request carries
`Authorization: Bearer <token>`. A request with an `Origin` header (a web page) is refused.
Bodies are JSON. An unknown field or a wrong type is an error and nothing happens.

Failures answer with a 4xx or 5xx status and:

```json
{"error": {"code": "rate_limited", "message": "A sentence for the person."}}
```

`account` is a linked number's digits, for example `447700900123`. `chat` and `to` accept a
chat id (`447700900111@s.whatsapp.net`, `1203...@g.us`), a phone number, or the exact name of
a chat or contact.

| Route | Body or query | Answer |
|---|---|---|
| `GET /health` | | `{"ok", "version", "accounts": [...]}` |
| `POST /shutdown` | | `{"ok": true}`, then the helper exits |
| `GET /accounts` | | `{"accounts": [{"account", "number", "name", "state", "detail", "usable"}]}` |
| `POST /accounts/remove` | `{"account"}` | Unlinks the number and deletes its keys, history and fetched files. `{"ok", "detail"}` |
| `POST /link/start` | `{"phone"}` | `{"link_id", "state": "pending", "code": "ABCD-EFGH", "qr", "expires_in"}` |
| `POST /link/status` | `{"link_id"}` | The same shape. `state` is `pending`, `linked` (with `account`) or `failed` (with `error`) |
| `GET /chats` | `account`, `query`, `unread=1`, `limit` | `{"chats": [{"chat", "name", "is_group", "unread", "last_time", "last_message"}], "state"}` |
| `GET /messages` | `account`, `chat`, `limit`, `before` | `{"chat", "messages": [...], "state"}`, oldest first |
| `GET /search` | `account`, `query`, `chat`, `limit` | `{"messages": [...], "state"}`, newest first |
| `GET /contacts` | `account`, `query`, `limit` | `{"contacts": [{"id", "number", "name", "profile_name", "has_chat", "is_group"}]}` |
| `POST /send` | `{"account", "to", "text", "reply_to", "file", "confirm_new_number", "approved"}` | `{"chat", "id", "time", "new_number"}` |
| `POST /read` | `{"account", "chat"}` | `{"chat", "marked"}` |
| `POST /media` | `{"account", "chat", "id"}` | `{"path", "kind", "media_type", "file_name"}` |

A message is `{"chat", "id", "sender", "sender_name", "from_me", "time", "kind", "text",
"reply_to", "media_type", "file_name"}`. `kind` is one of `text`, `image`, `video`, `audio`,
`voice note`, `file`, `sticker`, `location`, `contact card`, `view once`, `deleted`.

Account `state` is `connecting`, `connected`, `logged_out`, `restricted` or `outdated`.

## Sending

`POST /send` sends one message to one chat. `to` is a single string. `file` is a path on this
computer; JPEG and PNG go as images, anything else as a document, up to 50 MB. `text` is the
message, or the caption when there is a file. `reply_to` is the id of a message in that chat.

The request does not return until the message has gone, which includes the typing time, so
allow up to 30 seconds. Closing the connection before then cancels the send.

`approved` (boolean, default false) says the person explicitly approved this send in their
own words. It lifts the `broadcast` and `new_number_limit` refusals, the hourly and daily
`rate_limited` totals, and the need for `confirm_new_number`. It never lifts the pacing: the
typing time, the gap between sends and the per-minute `rate_limited` limit always apply. Set
it only from a check the assistant cannot answer for itself.

Refusal codes: `unknown_number`, `unknown_chat`, `broadcast`, `new_number_limit`,
`rate_limited`, `not_on_whatsapp`, `logged_out`, `restricted`, `outdated`, `offline`,
`send_failed`, `upload_failed`, `no_file`, `file_too_big`, `no_message`, `ambiguous`,
`no_chat`, `bad_chat`, `empty`, `cancelled`.

Codes from other routes: `no_account`, `bad_number`, `link_failed`, `no_link`, `no_media`,
`media_gone`, `store`, `bad_request`, `unauthorized`. Treat a code you do not know as a
refusal and show its `message`.

## Linking

`POST /link/start` opens a connection to WhatsApp and asks for a pairing code for that number.
The connection stays open inside the helper. The person enters the code on their phone
(WhatsApp, Settings, Linked devices, Link a device, Link with phone number instead). Poll
`POST /link/status` every few seconds. The code lasts about 160 seconds. `qr` carries the
current QR payload for a caller that can show one on another screen.

## Files in the folder

| File | What |
|---|---|
| `helper.json` | Port, token, pid, version |
| `session.db` | The linked-device keys for every linked number |
| `messages.db` | Chats, messages, contact names, the record of sends |
| `media/<account>/` | Files fetched with `POST /media` |
| `helper.log` | Operation names only. Never a message, a name or a number |
