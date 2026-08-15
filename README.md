# Google Drive Cloner Bot

A Go Telegram bot that clones Google Drive files/folders to a fixed destination ID using Google Drive API v3 server-side copy. It talks to Telegram over MTProto via [gotd/td](https://github.com/gotd/td).

## Features

- `/c <drive_link>` command trigger, accepting several links or IDs at once
- `/n <drive_link>` delete command trigger
- `/logs`, `/auth`, and `/unauth` admin commands, with authorizations persisted in SQLite
- Supports Google Drive file and folder links
- Destination always mapped to `GOOGLE_DRIVE_DESTINATION_ID`
- Server-side copying (no local file download/upload)
- Works with Shared Drives via `supportsAllDrives=true`
- Real-time live message updates with:
  - name and total size
  - percentage + progress bar
  - speed and ETA
  - monospace formatted status blocks
- Throttled edits (2.5s) to reduce Telegram rate-limit risk
- Handles common errors: invalid link, file not found, permission denied, rate limits, quota, copy restrictions, temporary Drive outages
- Console + rotating file logs at `logs/clonebot.log`

## Requirements

- Go 1.24+
- Telegram API credentials (`TELEGRAM_API_ID`, `TELEGRAM_API_HASH`) from https://my.telegram.org/apps
- Telegram bot token from BotFather (`TELEGRAM_BOT_TOKEN`)
- Google Drive API enabled in your Google Cloud project
- Google Drive auth:
  - One or more Service Account credentials, shared on private source/destination as needed
  - Optional OAuth client + refresh token fallback for files/drives only your Google account can access

## Setup

1. Build the binaries:

```bash
go build -o bin/clonebot ./cmd/clonebot
go build -o bin/oauthtoken ./cmd/oauthtoken
```

2. Configure environment:

```bash
cp .env.example .env
```

3. Edit `.env`:

- `TELEGRAM_API_ID`: Telegram API ID from `my.telegram.org/apps`
- `TELEGRAM_API_HASH`: Telegram API hash from `my.telegram.org/apps`
- `TELEGRAM_BOT_TOKEN`: Telegram bot token from BotFather
- `OWNER_ID`: your Telegram numeric user ID; commands from any other user are rejected
- `AUTHORIZED_CHAT_IDS`: optional comma-separated group/chat IDs where all members can use `/c`, `/s`, and `/n`. Use Bot API style IDs (for example `-1001234567890`)
- `GOOGLE_DRIVE_DESTINATION_ID`: destination folder ID (inside My Drive or Shared Drive)
- `SEARCH_REDACT_SECONDS`: optional; seconds before a search result message is redacted automatically. Defaults to `300`; set to `0` to disable
- `DATABASE_PATH`: optional; SQLite file storing IDs authorized at runtime with `/auth`. Defaults to `clonebot.db`
- Auth:
  - `SERVICE_ACCOUNT_JSON` as inline JSON, path to JSON file, or a JSON array of either
  - Optional fallback: `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REFRESH_TOKEN`
  - Optional multiple OAuth fallback accounts: `GOOGLE_OAUTH_CREDENTIALS` as a JSON array

## Run

```bash
go run ./cmd/clonebot
```

Or run the compiled binary:

```bash
./bin/clonebot
```

The bot keeps its Telegram session in memory and re-authenticates from the bot token on every start, so no session file is written.

### Quick start

`start.sh` builds the binary if needed and runs it in the foreground. Press Ctrl+C to stop.

```bash
./start.sh
```

Structured logs go to `logs/clonebot.log` as well as the console.

## Generate OAuth Refresh Token

Create an OAuth client in Google Cloud, then run one of these commands from the repo root.

With a downloaded desktop-client JSON file named `client_secret.json`:

```bash
go run ./cmd/oauthtoken
```

Or with the client ID and client secret directly:

```bash
go run ./cmd/oauthtoken -client-id "your-client-id" -client-secret "your-client-secret"
```

The command opens a browser login/consent flow (and prints the URL as a fallback), then prints the `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, and `GOOGLE_REFRESH_TOKEN` lines to add to `.env`. To add more normal Google accounts, run it once per account and place each result in `GOOGLE_OAUTH_CREDENTIALS`.

## Usage

In Telegram:

```text
/c https://drive.google.com/drive/folders/xxxx
/c https://drive.google.com/file/d/xxxx/view
/c 1AbCdEfGhIjKlMnOpQrStUvWxYz
/c 1AbCdEfGhIj 1XyZaBcDeFg 1MnOpQrStUv
/s movie name
/s folder name --dir
/s archive --all
/n https://drive.google.com/file/d/xxxx/view
/n https://drive.google.com/drive/folders/xxxx
/n 1AbCdEfGhIjKlMnOpQrStUvWxYz
/n 1AbCdEfGhIj 1XyZaBcDeFg
/server
/logs
/auth 123456789
/unauth 123456789
/restart
```

`/c` and `/n` accept several links, IDs, or search result IDs in one command, separated by spaces, commas, or new lines. The whole command gets a single status message: every item shows up as `QUEUED` with its name, and each block is rewritten in place as that item runs and finishes. At most 6 blocks are shown at once, in a window that follows the item being worked on, with `+N above` and `+N more` counting the rest. Items are processed one at a time. One bad ID does not stop the rest. Duplicates within a command are handled once.

`/server` reports host uptime, disk, CPU, and RAM. `/restart` re-executes the bot binary in place, keeping the same pid.

Only the Telegram user configured as `OWNER_ID` can use the admin commands (`/logs`, `/auth`, `/unauth`, `/restart`).

`/c`, `/n`, `/s`, `/server`, and `/help` are additionally available to:

- any chat listed in `AUTHORIZED_CHAT_IDS`
- any user or chat authorized at runtime with `/auth`

You can get your numeric Telegram user ID from bots such as `@userinfobot`.

## Authorization

```text
/auth            authorize the current chat
/auth 123456789  authorize a user or chat by ID
/auth            (as a reply) authorize the replied-to user
/unauth          revoke the current chat
/unauth [id]     revoke a user or chat by ID
/unauth          (as a reply) revoke the replied-to user
```

Replies to a user are answered with a `[User]` suffix so it is clear a person was authorized rather than a chat.

Runtime authorizations are stored in SQLite at `DATABASE_PATH`, so they survive restarts. IDs listed in `AUTHORIZED_CHAT_IDS` come from `.env` and cannot be revoked with `/unauth` — remove them from `.env` instead.

Search uses `/s <query>` and searches only Shared Drives visible to the configured Google identities. It does not search My Drive. By default, search returns files only. Use `--dir` at the end for folders only, or `--all` for files and folders. Results are sorted by reported size from largest to smallest and shown 5 per page with `PREV`, `NEXT`, and `CLOSE` buttons. Result messages redact themselves after `SEARCH_REDACT_SECONDS` (5 minutes by default) so Drive IDs do not linger in chat history. Search results show the title, size, and a copyable AES-encrypted ID; use that encrypted ID directly with `/c` to clone or `/n` to delete. Google Drive does not report recursive folder sizes in search results, so folders may show `Unknown`.

## Permissions Notes

- The bot tries service accounts first when `SERVICE_ACCOUNT_JSON` is configured.
- Multiple service accounts can be configured as paths:

```env
SERVICE_ACCOUNT_JSON=["./service-account-1.json","./service-account-2.json"]
```

- Or as inline credential objects:

```env
SERVICE_ACCOUNT_JSON=[{"type":"service_account","project_id":"..."},{"type":"service_account","project_id":"..."}]
```

- If OAuth credentials are also configured, it falls back to OAuth users when all service accounts get a 403/404 or a local permission/not-found check fails.
- One OAuth account can use the simple fields:

```env
GOOGLE_CLIENT_ID=...
GOOGLE_CLIENT_SECRET=...
GOOGLE_REFRESH_TOKEN=...
```

- Multiple OAuth accounts can use a JSON array:

```env
GOOGLE_OAUTH_CREDENTIALS=[
  {"client_id":"...","client_secret":"...","refresh_token":"..."},
  {"client_id":"...","client_secret":"...","refresh_token":"..."}
]
```

- Auth order is service accounts first, then OAuth accounts in configured order.
- For private source links, the active identity must have at least Viewer access.
- Destination folder must grant Editor access to the same active identity.
- If cloning to Shared Drive, ensure the active identity is a Shared Drive member with write permission.

## Layout

```text
cmd/clonebot      bot entry point: logging, config, Telegram client
cmd/oauthtoken    OAuth consent flow helper
internal/config   .env loading and validation
internal/drive    Drive client: auth fallback, clone, search, delete
internal/progress live progress rendering and edit throttling
internal/bot      command routing, authorization, search pagination
internal/store    SQLite store for runtime authorizations
```

## Development

```bash
go test ./...
go vet ./...
gofmt -l .
```

## Limitations

- Google Drive API copy is server-side but does not expose byte-level progress for a single large file; progress updates are most accurate per-file and across folders.
- Native Google Docs/Sheets/Slides report `size=0` from API metadata.
