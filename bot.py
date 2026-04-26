from __future__ import annotations

import asyncio
import html
import logging
from logging.handlers import RotatingFileHandler
from pathlib import Path
from typing import Optional

from pyrogram import Client, enums, filters, types
from rich.logging import RichHandler

from clonebot.config import ConfigError, load_config
from clonebot.drive import DriveCloneError, DriveCloner, DriveDuplicateError
from clonebot.progress import CloneProgress, ThrottledReporter

logger = logging.getLogger("drive-clone-bot")
_LINK_PREVIEW_OFF = types.LinkPreviewOptions(is_disabled=True)
_LOG_DIR = Path("logs")
_LOG_FILE = _LOG_DIR / "clonebot.log"


def _configure_app_logging() -> None:
    _LOG_DIR.mkdir(parents=True, exist_ok=True)
    file_formatter = logging.Formatter("%(asctime)s %(levelname)s [%(name)s] %(message)s")
    console_formatter = logging.Formatter("%(message)s")

    root_logger = logging.getLogger()
    root_logger.handlers.clear()
    root_logger.setLevel(logging.INFO)

    stream_handler = RichHandler(
        rich_tracebacks=True,
        show_path=False,
        markup=False,
        log_time_format="[%H:%M:%S]",
    )
    stream_handler.setLevel(logging.INFO)
    stream_handler.setFormatter(console_formatter)

    file_handler = RotatingFileHandler(
        _LOG_FILE,
        maxBytes=5 * 1024 * 1024,
        backupCount=5,
        encoding="utf-8",
    )
    file_handler.setLevel(logging.INFO)
    file_handler.setFormatter(file_formatter)

    root_logger.addHandler(stream_handler)
    root_logger.addHandler(file_handler)


def _configure_library_logging() -> None:
    # Keep bot logs at INFO while reducing noisy internals from Kurigram/Pyrogram.
    logging.getLogger("pyrogram").setLevel(logging.WARNING)
    logging.getLogger("pyrogram.connection").setLevel(logging.WARNING)
    logging.getLogger("pyrogram.session").setLevel(logging.WARNING)


HELP_TEXT = (
    "Google Drive Cloner Bot\n\n"
    "Commands:\n"
    "/c <google_drive_file_or_folder_link>  Clone to configured destination\n"
    "/n <google_drive_file_or_folder_link>  Delete file/folder by link\n\n"
    "Destination is fixed by GOOGLE_DRIVE_DESTINATION_ID from .env."
)


def _extract_source_link(message) -> Optional[str]:
    command = getattr(message, "command", None) or []
    if len(command) < 2:
        return None
    source_link = command[1].strip()
    return source_link or None


def _message_context(message) -> str:
    chat = getattr(message, "chat", None)
    user = getattr(message, "from_user", None)
    chat_id = getattr(chat, "id", None)
    user_id = getattr(user, "id", None)
    username = getattr(user, "username", None)
    return f"chat_id={chat_id} user_id={user_id} username={username}"


def _is_owner(client: Client, message) -> bool:
    user = getattr(message, "from_user", None)
    user_id = getattr(user, "id", None)
    return user_id == client.clonebot_config.owner_id


async def _reject_non_owner(client: Client, message) -> bool:
    if _is_owner(client, message):
        return False
    logger.warning("Unauthorized command rejected: %s", _message_context(message))
    await message.reply_text("<code>UNAUTHORIZED</code>")
    return True


async def start_command(client: Client, message) -> None:
    if await _reject_non_owner(client, message):
        return
    await message.reply_text(HELP_TEXT)


async def help_command(client: Client, message) -> None:
    if await _reject_non_owner(client, message):
        return
    await message.reply_text(HELP_TEXT)


async def clone_command(client: Client, message) -> None:
    if await _reject_non_owner(client, message):
        return
    ctx = _message_context(message)
    source_link = _extract_source_link(message)
    if not source_link:
        await message.reply_text("Usage: /c <google_drive_link>")
        return

    logger.info("Clone requested: %s link=%s", ctx, source_link)

    cfg = client.clonebot_config
    loop = asyncio.get_running_loop()
    progress_msg = await message.reply_text("<code>CHECKING..</code>")

    try:
        precheck = await asyncio.to_thread(_run_clone_prepare, cfg, source_link)
    except DriveDuplicateError as exc:
        existing = exc.existing
        safe_url = html.escape(existing.get("url", ""), quote=True)
        logger.warning(
            "Clone precheck duplicate: %s link=%s existing_id=%s name=%s url=%s",
            ctx,
            source_link,
            existing.get("id"),
            existing.get("name"),
            existing.get("url"),
        )
        if safe_url:
            await progress_msg.edit_text(
                f"<code>FILE EXISTS • </code><a href=\"{safe_url}\"><b>DL</b></a>",
                link_preview_options=_LINK_PREVIEW_OFF,
            )
        else:
            await progress_msg.edit_text("<code>FILE EXISTS</code>")
        return
    except DriveCloneError as exc:
        msg = _simple_error_text(str(exc))
        logger.warning("Clone precheck failed: %s link=%s reason=%s", ctx, source_link, msg)
        await progress_msg.edit_text(f"<code>{html.escape(msg)}</code>")
        return
    except Exception:
        logger.exception("Unhandled clone precheck failure: %s link=%s", ctx, source_link)
        await progress_msg.edit_text("<code>UNEXPECTED ERROR</code>")
        return
    logger.info(
        "Clone precheck ok: %s link=%s source_id=%s name=%s mime=%s auth=%s",
        ctx,
        source_link,
        precheck.get("source_id"),
        precheck.get("name"),
        precheck.get("source_mime_type"),
        precheck.get("auth_mode"),
    )

    progress = CloneProgress(task_name=precheck.get("name", "Unnamed"))
    throttler = ThrottledReporter(min_interval_seconds=2.5)
    edit_lock = asyncio.Lock()
    done = False

    async def edit_progress(text: str, final: bool = False) -> bool:
        nonlocal done
        async with edit_lock:
            if done and not final:
                return False
            try:
                await progress_msg.edit_text(
                    text,
                    link_preview_options=_LINK_PREVIEW_OFF,
                )
                return True
            except Exception as exc:
                msg = str(exc).upper()
                if "MESSAGE_NOT_MODIFIED" not in msg and "MESSAGE IS NOT MODIFIED" not in msg:
                    logger.warning("Failed to edit progress message: %s", exc)
                return False

    def on_progress(prg: CloneProgress, force: bool = False) -> None:
        nonlocal done
        if done:
            return
        if not throttler.should_update(force=force):
            return
        asyncio.run_coroutine_threadsafe(edit_progress(prg.as_message()), loop)

    await edit_progress(progress.as_message())
    logger.info("Clone started: %s link=%s destination_id=%s", ctx, source_link, cfg.destination_id)

    try:
        result = await asyncio.to_thread(
            _run_clone,
            cfg,
            source_link,
            precheck.get("auth_mode"),
            progress,
            on_progress,
        )
    except DriveDuplicateError as exc:
        done = True
        existing = exc.existing
        safe_url = html.escape(existing.get("url", ""), quote=True)
        logger.warning(
            "Clone duplicate at copy-time: %s link=%s existing_id=%s name=%s url=%s",
            ctx,
            source_link,
            existing.get("id"),
            existing.get("name"),
            existing.get("url"),
        )
        if safe_url:
            await edit_progress(
                f"<code>FILE EXISTS • </code><a href=\"{safe_url}\"><b>DL</b></a>",
                final=True,
            )
        else:
            await edit_progress("<code>FILE EXISTS</code>", final=True)
        return
    except DriveCloneError as exc:
        done = True
        msg = _simple_error_text(str(exc))
        logger.warning("Clone failed: %s link=%s reason=%s", ctx, source_link, msg)
        await edit_progress(f"<code>{html.escape(msg)}</code>", final=True)
        return
    except Exception:
        logger.exception("Unhandled clone failure: %s link=%s", ctx, source_link)
        done = True
        await edit_progress("Clone failed due to an unexpected error.", final=True)
        return

    if not isinstance(result, dict) or "name" not in result or "url" not in result:
        logger.error("Clone returned invalid result: %s link=%s result=%r", ctx, source_link, result)
        done = True
        await edit_progress("Clone failed: invalid clone result received.", final=True)
        return
    logger.info(
        "Clone completed: %s source_link=%s cloned_id=%s name=%s mime=%s url=%s",
        ctx,
        source_link,
        result.get("id"),
        result.get("name"),
        result.get("mime_type", "unknown"),
        result.get("url"),
    )

    done = True
    final_updated = await edit_progress(
        progress.completion_message(
            result["name"],
            result["url"],
            result.get("mime_type", "unknown"),
        ),
        final=True,
    )
    if final_updated:
        logger.info("Clone final message updated: %s cloned_id=%s name=%s", ctx, result.get("id"), result.get("name"))
    else:
        logger.warning("Clone final message update was skipped or failed: %s cloned_id=%s name=%s", ctx, result.get("id"), result.get("name"))


async def delete_command(client: Client, message) -> None:
    if await _reject_non_owner(client, message):
        return
    ctx = _message_context(message)
    source_link = _extract_source_link(message)
    if not source_link:
        await message.reply_text("Usage: /n <google_drive_link>")
        return

    logger.info("Nuke requested: %s link=%s", ctx, source_link)

    cfg = client.clonebot_config
    deleting_msg = await message.reply_text("<code>CHECKING..</code>")

    try:
        target = await asyncio.to_thread(_run_delete_prepare, cfg, source_link)
    except DriveCloneError as exc:
        raw_msg = str(exc)
        msg = html.escape(raw_msg)
        logger.warning("Nuke precheck failed: %s link=%s reason=%s", ctx, source_link, raw_msg)
        if raw_msg == "FILE DOESN'T EXIST":
            await deleting_msg.edit_text(f"<code>{msg}</code>")
        else:
            await deleting_msg.edit_text(f"<code>CHECKING..</code>\n<code>{msg}</code>")
        return
    except Exception:
        logger.exception("Unhandled nuke precheck failure: %s link=%s", ctx, source_link)
        await deleting_msg.edit_text("<code>CHECKING..</code>\n<code>UNEXPECTED ERROR</code>")
        return
    logger.info(
        "Nuke precheck ok: %s link=%s target_id=%s name=%s mode=%s mime=%s",
        ctx,
        source_link,
        target.get("id"),
        target.get("name"),
        target.get("mode"),
        target.get("mimeType"),
    )

    await deleting_msg.edit_text("<code>NUKING..</code>")

    try:
        result = await asyncio.to_thread(_run_delete_commit, cfg, target)
    except DriveCloneError as exc:
        raw_msg = str(exc)
        msg = html.escape(raw_msg)
        logger.warning("Nuke failed: %s link=%s reason=%s", ctx, source_link, raw_msg)
        if raw_msg == "FILE DOESN'T EXIST":
            await deleting_msg.edit_text(f"<code>{msg}</code>")
        else:
            await deleting_msg.edit_text(f"<code>CHECKING..</code>\n<code>{msg}</code>")
        return
    except Exception:
        logger.exception("Unhandled nuke failure: %s link=%s", ctx, source_link)
        await deleting_msg.edit_text("<code>CHECKING..</code>\n<code>UNEXPECTED ERROR</code>")
        return
    logger.info(
        "Nuke completed: %s source_link=%s target_id=%s name=%s mode=%s mime=%s",
        ctx,
        source_link,
        result.get("id"),
        result.get("name"),
        target.get("mode"),
        target.get("mimeType"),
    )

    await deleting_msg.edit_text(
        f"<code>NUKED: {html.escape(result['name'])}</code>",
    )



def _run_clone(cfg, source_link: str, auth_mode: str | None, progress: CloneProgress, on_progress):
    cloner = DriveCloner(cfg, preferred_auth=auth_mode)
    return cloner.clone(
        source_link=source_link,
        destination_id=cfg.destination_id,
        progress=progress,
        progress_cb=on_progress,
    )



def _run_clone_prepare(cfg, source_link: str):
    cloner = DriveCloner(cfg)
    return cloner.prepare_clone(source_link=source_link, destination_id=cfg.destination_id)



def _run_delete(cfg, source_link: str):
    cloner = DriveCloner(cfg)
    return cloner.delete(source_link=source_link)



def _run_delete_prepare(cfg, source_link: str):
    cloner = DriveCloner(cfg)
    return cloner.prepare_delete(source_link=source_link)



def _run_delete_commit(cfg, target: dict):
    cloner = DriveCloner(cfg, preferred_auth=target.get("auth_mode"))
    return cloner.perform_delete(target=target)



def _simple_error_text(raw: str) -> str:
    msg = (raw or "").strip()
    upper = msg.upper()
    if "FILE EXISTS" in upper or "ALREADY EXISTS" in upper:
        return "FILE EXISTS"
    if "FILE DOESN'T EXIST" in upper or "NOT FOUND" in upper:
        return "FILE DOESN'T EXIST"
    if "YOU DON'T HAVE PERMS" in upper or "PERMISSION" in upper or "403" in upper:
        return "YOU DON'T HAVE PERMS"
    return msg or "UNEXPECTED ERROR"



def main() -> None:
    _configure_app_logging()
    _configure_library_logging()

    try:
        config = load_config()
    except ConfigError as exc:
        raise SystemExit(f"Configuration error: {exc}") from exc

    app = Client(
        "clonebot_bot",
        api_id=config.telegram_api_id,
        api_hash=config.telegram_api_hash,
        bot_token=config.telegram_bot_token,
        parse_mode=enums.ParseMode.HTML,
        in_memory=True,
    )
    app.clonebot_config = config

    app.on_message(filters.command("start"))(start_command)
    app.on_message(filters.command("help"))(help_command)
    app.on_message(filters.command("c"))(clone_command)
    app.on_message(filters.command("n"))(delete_command)

    logger.info("Logging to %s", _LOG_FILE.resolve())
    logger.info("Bot started")
    try:
        app.run()
    except KeyboardInterrupt:
        logger.info("Shutdown requested (Ctrl+C)")
    finally:
        logger.info("Bot stopped")


if __name__ == "__main__":
    main()
