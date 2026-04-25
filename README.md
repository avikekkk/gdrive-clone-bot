# Google Drive Kurigram Cloner Bot

A Kurigram-based Telegram bot that clones Google Drive files/folders to a fixed destination ID using Google Drive API v3 server-side copy.

## Features

- `/c <drive_link>` command trigger
- `/n <drive_link>` delete command trigger
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
- Handles common errors: invalid link, 403 permission, storage quota, 404
- Console + rotating file logs at `logs/clonebot.log`

## Requirements

- Python 3.10+
- Kurigram bot runtime
- Telegram API credentials (`TELEGRAM_API_ID`, `TELEGRAM_API_HASH`) from https://my.telegram.org/apps
- Telegram bot token from BotFather (`TELEGRAM_BOT_TOKEN`)
- Google Drive API enabled in your Google Cloud project
- Either:
  - Service Account credentials, shared on private source/destination as needed, or
  - OAuth client + refresh token

## Setup (uv)

1. Install `uv` (if not already installed):

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

2. Sync dependencies and create `.venv`:

```bash
uv sync
```

If `uv run clonebot` says `Failed to spawn: clonebot`, reinstall once as non-editable:

```bash
uv sync --no-editable
```

3. Configure environment:

```bash
cp .env.example .env
```

4. Edit `.env`:

- `TELEGRAM_API_ID`: Telegram API ID from `my.telegram.org/apps`
- `TELEGRAM_API_HASH`: Telegram API hash from `my.telegram.org/apps`
- `TELEGRAM_BOT_TOKEN`: Telegram bot token from BotFather
- `GOOGLE_DRIVE_DESTINATION_ID`: destination folder ID (inside My Drive or Shared Drive)
- Auth (choose one):
  - `SERVICE_ACCOUNT_JSON` as inline JSON or path to JSON file
  - or `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REFRESH_TOKEN`

## Run

```bash
uv run clonebot
```

Alternative run command:

```bash
uv run python bot.py
```

## Setup (pip fallback)

```bash
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
```

## Usage

In Telegram:

```text
/c https://drive.google.com/drive/folders/xxxx
/c https://drive.google.com/file/d/xxxx/view
/n https://drive.google.com/file/d/xxxx/view
/n https://drive.google.com/drive/folders/xxxx
```

## Permissions Notes

- For private source links, the service account (or OAuth user) must have at least Viewer access.
- Destination folder must grant Editor access to the same identity.
- If cloning to Shared Drive, ensure the identity is a Shared Drive member with write permission.

## Limitations

- Google Drive API copy is server-side but does not expose byte-level progress for a single large file; progress updates are most accurate per-file and across folders.
- Native Google Docs/Sheets/Slides report `size=0` from API metadata.
