from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any, Optional

from dotenv import load_dotenv


class ConfigError(RuntimeError):
    """Raised when required environment configuration is missing."""


@dataclass(frozen=True)
class OAuthCredentials:
    client_id: str
    client_secret: str
    refresh_token: str


@dataclass(frozen=True)
class BotConfig:
    telegram_api_id: int
    telegram_api_hash: str
    telegram_bot_token: str
    owner_id: int
    authorized_chat_ids: tuple[int, ...]
    destination_id: str
    service_account_json: Optional[str]
    service_account_jsons: tuple[str, ...]
    google_client_id: Optional[str]
    google_client_secret: Optional[str]
    google_refresh_token: Optional[str]
    google_oauth_credentials: tuple[OAuthCredentials, ...]



def _clean(value: Optional[str]) -> Optional[str]:
    if value is None:
        return None
    value = value.strip()
    return value if value else None


def _normalize_service_account_entry(value: Any, env_name: str) -> str:
    if isinstance(value, dict):
        return json.dumps(value, separators=(",", ":"))
    if not isinstance(value, str):
        raise ConfigError(f"{env_name} entries must be JSON objects or strings")

    value = value.strip()
    if not value:
        raise ConfigError(f"{env_name} contains an empty service account entry")
    return value


def _parse_service_account_entries(env_name: str, value: Optional[str]) -> tuple[str, ...]:
    if not value:
        return ()

    if value.startswith("["):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError as exc:
            raise ConfigError(f"{env_name} must be a valid JSON array") from exc
        if not isinstance(parsed, list):
            raise ConfigError(f"{env_name} must be a JSON array")
        return tuple(
            _normalize_service_account_entry(entry, env_name) for entry in parsed
        )

    if value.startswith("{"):
        return (_normalize_service_account_entry(value, env_name),)

    return tuple(
        _normalize_service_account_entry(line, env_name)
        for line in value.splitlines()
        if line.strip()
    )


def _validate_service_account_entry(value: str, env_name: str) -> None:
    if value.startswith("{"):
        try:
            json.loads(value)
        except json.JSONDecodeError as exc:
            raise ConfigError(f"{env_name} must contain valid JSON credentials") from exc
    else:
        if not os.path.exists(value):
            raise ConfigError(
                f"{env_name} path does not exist. Use absolute or project-relative path."
            )


def _parse_oauth_credentials(value: Optional[str]) -> tuple[OAuthCredentials, ...]:
    if not value:
        return ()

    try:
        parsed = json.loads(value)
    except json.JSONDecodeError as exc:
        raise ConfigError("GOOGLE_OAUTH_CREDENTIALS must be a valid JSON array") from exc

    if not isinstance(parsed, list):
        raise ConfigError("GOOGLE_OAUTH_CREDENTIALS must be a JSON array")

    credentials = []
    for idx, item in enumerate(parsed, start=1):
        if not isinstance(item, dict):
            raise ConfigError(f"GOOGLE_OAUTH_CREDENTIALS entry #{idx} must be an object")

        client_id = _clean(item.get("client_id"))
        client_secret = _clean(item.get("client_secret"))
        refresh_token = _clean(item.get("refresh_token"))

        if not client_id or not client_secret or not refresh_token:
            raise ConfigError(
                f"GOOGLE_OAUTH_CREDENTIALS entry #{idx} must include client_id, client_secret, and refresh_token"
            )

        credentials.append(
            OAuthCredentials(
                client_id=client_id,
                client_secret=client_secret,
                refresh_token=refresh_token,
            )
        )

    return tuple(credentials)


def _parse_authorized_chat_ids(value: Optional[str]) -> tuple[int, ...]:
    if not value:
        return ()

    parts = [part.strip() for part in value.replace("\n", ",").split(",")]
    chat_ids = []
    for part in parts:
        if not part:
            continue
        try:
            chat_ids.append(int(part))
        except ValueError as exc:
            raise ConfigError("AUTHORIZED_CHAT_IDS must be a comma-separated list of chat IDs") from exc
    return tuple(chat_ids)



def load_config() -> BotConfig:
    load_dotenv()

    telegram_api_id_raw = _clean(os.getenv("TELEGRAM_API_ID"))
    telegram_api_hash = _clean(os.getenv("TELEGRAM_API_HASH"))
    telegram_bot_token = _clean(os.getenv("TELEGRAM_BOT_TOKEN"))
    owner_id_raw = _clean(os.getenv("OWNER_ID"))
    authorized_chat_ids_raw = _clean(os.getenv("AUTHORIZED_CHAT_IDS"))
    destination_id = _clean(os.getenv("GOOGLE_DRIVE_DESTINATION_ID"))

    service_account_json = _clean(os.getenv("SERVICE_ACCOUNT_JSON"))
    service_account_jsons_raw = _clean(os.getenv("SERVICE_ACCOUNT_JSONS"))
    google_client_id = _clean(os.getenv("GOOGLE_CLIENT_ID"))
    google_client_secret = _clean(os.getenv("GOOGLE_CLIENT_SECRET"))
    google_refresh_token = _clean(os.getenv("GOOGLE_REFRESH_TOKEN"))
    google_oauth_credentials_raw = _clean(os.getenv("GOOGLE_OAUTH_CREDENTIALS"))

    if not telegram_api_id_raw:
        raise ConfigError("Missing TELEGRAM_API_ID in .env")
    if not telegram_api_hash:
        raise ConfigError("Missing TELEGRAM_API_HASH in .env")
    if not telegram_bot_token:
        raise ConfigError("Missing TELEGRAM_BOT_TOKEN in .env")
    if not owner_id_raw:
        raise ConfigError("Missing OWNER_ID in .env")
    if not destination_id:
        raise ConfigError("Missing GOOGLE_DRIVE_DESTINATION_ID in .env")

    try:
        telegram_api_id = int(telegram_api_id_raw)
    except ValueError as exc:
        raise ConfigError("TELEGRAM_API_ID must be an integer") from exc

    try:
        owner_id = int(owner_id_raw)
    except ValueError as exc:
        raise ConfigError("OWNER_ID must be a Telegram numeric user ID") from exc

    authorized_chat_ids = _parse_authorized_chat_ids(authorized_chat_ids_raw)

    service_account_jsons = _parse_service_account_entries(
        "SERVICE_ACCOUNT_JSON", service_account_json
    ) + _parse_service_account_entries(
        "SERVICE_ACCOUNT_JSONS", service_account_jsons_raw
    )

    google_oauth_credentials = _parse_oauth_credentials(google_oauth_credentials_raw)
    if google_client_id or google_client_secret or google_refresh_token:
        if not all([google_client_id, google_client_secret, google_refresh_token]):
            raise ConfigError(
                "GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, and GOOGLE_REFRESH_TOKEN must be provided together"
            )
        google_oauth_credentials = (
            OAuthCredentials(
                client_id=google_client_id,
                client_secret=google_client_secret,
                refresh_token=google_refresh_token,
            ),
        ) + google_oauth_credentials

    has_service_account = bool(service_account_jsons)
    has_oauth = bool(google_oauth_credentials)

    if not has_service_account and not has_oauth:
        raise ConfigError(
            "Provide SERVICE_ACCOUNT_JSON and/or GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET/GOOGLE_REFRESH_TOKEN or GOOGLE_OAUTH_CREDENTIALS"
        )

    # Service account entries accept either inline JSON or a file path.
    for idx, entry in enumerate(service_account_jsons, start=1):
        _validate_service_account_entry(entry, f"service account #{idx}")

    return BotConfig(
        telegram_api_id=telegram_api_id,
        telegram_api_hash=telegram_api_hash,
        telegram_bot_token=telegram_bot_token,
        owner_id=owner_id,
        authorized_chat_ids=authorized_chat_ids,
        destination_id=destination_id,
        service_account_json=service_account_json,
        service_account_jsons=service_account_jsons,
        google_client_id=google_client_id,
        google_client_secret=google_client_secret,
        google_refresh_token=google_refresh_token,
        google_oauth_credentials=google_oauth_credentials,
    )
