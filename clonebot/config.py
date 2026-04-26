from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Optional

from dotenv import load_dotenv


class ConfigError(RuntimeError):
    """Raised when required environment configuration is missing."""


@dataclass(frozen=True)
class BotConfig:
    telegram_api_id: int
    telegram_api_hash: str
    telegram_bot_token: str
    owner_id: int
    destination_id: str
    service_account_json: Optional[str]
    google_client_id: Optional[str]
    google_client_secret: Optional[str]
    google_refresh_token: Optional[str]



def _clean(value: Optional[str]) -> Optional[str]:
    if value is None:
        return None
    value = value.strip()
    return value if value else None



def load_config() -> BotConfig:
    load_dotenv()

    telegram_api_id_raw = _clean(os.getenv("TELEGRAM_API_ID"))
    telegram_api_hash = _clean(os.getenv("TELEGRAM_API_HASH"))
    telegram_bot_token = _clean(os.getenv("TELEGRAM_BOT_TOKEN"))
    owner_id_raw = _clean(os.getenv("OWNER_ID"))
    destination_id = _clean(os.getenv("GOOGLE_DRIVE_DESTINATION_ID"))

    service_account_json = _clean(os.getenv("SERVICE_ACCOUNT_JSON"))
    google_client_id = _clean(os.getenv("GOOGLE_CLIENT_ID"))
    google_client_secret = _clean(os.getenv("GOOGLE_CLIENT_SECRET"))
    google_refresh_token = _clean(os.getenv("GOOGLE_REFRESH_TOKEN"))

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

    has_service_account = bool(service_account_json)
    has_oauth = all([google_client_id, google_client_secret, google_refresh_token])

    if not has_service_account and not has_oauth:
        raise ConfigError(
            "Provide SERVICE_ACCOUNT_JSON and/or GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET/GOOGLE_REFRESH_TOKEN"
        )

    # SERVICE_ACCOUNT_JSON accepts either inline JSON or a file path.
    if service_account_json:
        if service_account_json.startswith("{"):
            try:
                json.loads(service_account_json)
            except json.JSONDecodeError as exc:
                raise ConfigError("SERVICE_ACCOUNT_JSON must be valid JSON") from exc
        else:
            if not os.path.exists(service_account_json):
                raise ConfigError(
                    "SERVICE_ACCOUNT_JSON path does not exist. Use absolute or project-relative path."
                )

    return BotConfig(
        telegram_api_id=telegram_api_id,
        telegram_api_hash=telegram_api_hash,
        telegram_bot_token=telegram_bot_token,
        owner_id=owner_id,
        destination_id=destination_id,
        service_account_json=service_account_json,
        google_client_id=google_client_id,
        google_client_secret=google_client_secret,
        google_refresh_token=google_refresh_token,
    )
