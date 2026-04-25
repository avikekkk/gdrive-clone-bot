from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from google_auth_oauthlib.flow import InstalledAppFlow


SCOPES = ["https://www.googleapis.com/auth/drive"]


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Generate Google OAuth credentials for the Drive fallback auth."
    )
    parser.add_argument(
        "--client-secret-file",
        default="client_secret.json",
        help="Path to the OAuth desktop client JSON downloaded from Google Cloud.",
    )
    parser.add_argument(
        "--client-id",
        help="OAuth client ID. Use this with --client-secret if you do not have a JSON file.",
    )
    parser.add_argument(
        "--client-secret",
        help="OAuth client secret. Use this with --client-id if you do not have a JSON file.",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=0,
        help="Local callback port. Use 0 to pick a free port automatically.",
    )
    args = parser.parse_args()

    try:
        flow = build_flow(args)
        creds = flow.run_local_server(port=args.port, prompt="consent")
    except Exception as exc:
        print(f"Failed to generate refresh token: {exc}", file=sys.stderr)
        return 1

    if not creds.refresh_token:
        print(
            "Google did not return a refresh token. Re-run with the same command and make sure "
            "you approve the consent prompt.",
            file=sys.stderr,
        )
        return 1

    print()
    print("Add these lines to .env:")
    print(f"GOOGLE_CLIENT_ID={creds.client_id}")
    print(f"GOOGLE_CLIENT_SECRET={creds.client_secret}")
    print(f"GOOGLE_REFRESH_TOKEN={creds.refresh_token}")
    return 0


def build_flow(args: argparse.Namespace) -> InstalledAppFlow:
    if args.client_id or args.client_secret:
        if not args.client_id or not args.client_secret:
            raise ValueError("--client-id and --client-secret must be provided together")
        return InstalledAppFlow.from_client_config(
            {
                "installed": {
                    "client_id": args.client_id,
                    "client_secret": args.client_secret,
                    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
                    "token_uri": "https://oauth2.googleapis.com/token",
                    "redirect_uris": ["http://localhost"],
                }
            },
            SCOPES,
        )

    client_secret_file = Path(args.client_secret_file)
    if not client_secret_file.exists():
        raise FileNotFoundError(
            f"{client_secret_file} does not exist. Download an OAuth desktop client JSON "
            "from Google Cloud or pass --client-id and --client-secret."
        )

    validate_client_secret_file(client_secret_file)
    return InstalledAppFlow.from_client_secrets_file(str(client_secret_file), SCOPES)


def validate_client_secret_file(path: Path) -> None:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as exc:
        raise ValueError(f"{path} is not valid JSON") from exc

    if "installed" not in data and "web" not in data:
        raise ValueError(
            f"{path} does not look like a Google OAuth client secret JSON file"
        )


if __name__ == "__main__":
    raise SystemExit(main())
