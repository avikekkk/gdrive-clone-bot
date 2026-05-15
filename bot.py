from __future__ import annotations

import asyncio
import html
import logging
import shlex
import uuid
from logging.handlers import RotatingFileHandler
from pathlib import Path
from typing import Optional

from pyrogram import Client, enums, filters, types
from pyrogram.types import InlineKeyboardButton, InlineKeyboardMarkup
from rich.logging import RichHandler

from clonebot.config import ConfigError, load_config
from clonebot.drive import DriveCloneError, DriveCloner, DriveDuplicateError
from clonebot.progress import CloneProgress, ThrottledReporter, _fmt_bytes

logger = logging.getLogger("drive-clone-bot")
_LINK_PREVIEW_OFF = types.LinkPreviewOptions(is_disabled=True)
_LOG_DIR = Path("logs")
_LOG_FILE = _LOG_DIR / "clonebot.log"
SEARCH_RESULTS_PER_PAGE = 5
MAX_SEARCH_SESSIONS = 100
SEARCH_SESSIONS = {}


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
    "/s <search_query> [--dir|--all]  Search Shared Drives visible to configured Drive accounts\n"
    "/n <google_drive_file_or_folder_link>  Delete file/folder by link\n\n"
    "Destination is fixed by GOOGLE_DRIVE_DESTINATION_ID from .env."
)


def _extract_source_link(message) -> Optional[str]:
    command = getattr(message, "command", None) or []
    if len(command) < 2:
        return None
    source_link = command[1].strip()
    return source_link or None


def _extract_command_payload(message) -> Optional[str]:
    text = getattr(message, "text", None) or getattr(message, "caption", None) or ""
    parts = text.split(maxsplit=1)
    if len(parts) < 2:
        return None
    payload = parts[1].strip()
    return payload or None


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


def _is_authorized_clone_chat(client: Client, message) -> bool:
    chat = getattr(message, "chat", None)
    chat_id = getattr(chat, "id", None)
    return chat_id in client.clonebot_config.authorized_chat_ids


async def _reject_non_owner(client: Client, message) -> bool:
    if _is_owner(client, message):
        return False
    logger.warning("Unauthorized command rejected: %s", _message_context(message))
    await message.reply_text("<code>UNAUTHORIZED</code>")
    return True


async def _reject_unauthorized_clone(client: Client, message) -> bool:
    if _is_owner(client, message) or _is_authorized_clone_chat(client, message):
        return False
    logger.warning("Unauthorized clone command rejected: %s", _message_context(message))
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
    if await _reject_unauthorized_clone(client, message):
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
        msg = _simple_error_text(str(exc))
        logger.warning("Nuke precheck failed: %s link=%s reason=%s", ctx, source_link, msg)
        if msg == "FILE DOESN'T EXIST":
            await deleting_msg.edit_text(f"<code>{msg}</code>")
        else:
            await deleting_msg.edit_text(f"<code>CHECKING..</code>\n<code>{html.escape(msg)}</code>")
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
        msg = _simple_error_text(str(exc))
        logger.warning("Nuke failed: %s link=%s reason=%s", ctx, source_link, msg)
        if msg == "FILE DOESN'T EXIST":
            await deleting_msg.edit_text(f"<code>{msg}</code>")
        else:
            await deleting_msg.edit_text(f"<code>CHECKING..</code>\n<code>{html.escape(msg)}</code>")
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
        f"<u><b>NUKED:</b></u>\n<code>{html.escape(result['name'])}</code>",
    )


async def search_command(client: Client, message) -> None:
    if await _reject_unauthorized_clone(client, message):
        return
    ctx = _message_context(message)
    payload = _extract_command_payload(message)
    if not payload:
        await message.reply_text("Usage: /s <search_query>")
        return
    query, item_type, parse_error = _parse_search_args(payload)
    if parse_error:
        await message.reply_text(parse_error)
        return
    if not query:
        await message.reply_text("Usage: /s <search_query>")
        return

    logger.info("Search requested: %s query=%s type=%s", ctx, query, item_type)
    searching_msg = await message.reply_text("<code>SEARCHING..</code>")

    try:
        results = await asyncio.to_thread(_run_search, client.clonebot_config, query, item_type)
    except DriveCloneError as exc:
        msg = _simple_error_text(str(exc))
        logger.warning("Search failed: %s query=%s reason=%s", ctx, query, msg)
        await searching_msg.edit_text(f"<code>{html.escape(msg)}</code>")
        return
    except Exception:
        logger.exception("Unhandled search failure: %s query=%s", ctx, query)
        await searching_msg.edit_text("<code>UNEXPECTED ERROR</code>")
        return

    if not results:
        logger.info("Search completed: %s query=%s results=0", ctx, query)
        await searching_msg.edit_text(_format_search_results(query, results))
        return

    token = uuid.uuid4().hex[:8]
    user = getattr(message, "from_user", None)
    SEARCH_SESSIONS[token] = {
        "results": results,
        "query": query,
        "item_type": item_type,
        "user_id": getattr(user, "id", None),
    }
    if len(SEARCH_SESSIONS) > MAX_SEARCH_SESSIONS:
        SEARCH_SESSIONS.pop(next(iter(SEARCH_SESSIONS)))

    text, total_pages, page = _build_search_page_text(results, query, 0)
    markup = _build_search_page_markup(token, page, total_pages)
    logger.info("Search completed: %s query=%s results=%s", ctx, query, len(results))
    await searching_msg.edit_text(
        text,
        reply_markup=markup,
        link_preview_options=_LINK_PREVIEW_OFF,
    )


async def search_pagination_callback(client: Client, callback_query) -> None:
    data = callback_query.data or ""

    if data == "gdsnoop":
        await callback_query.answer()
        return

    if data.startswith("gdsc:"):
        token = data.split(":", 1)[1]
        session = SEARCH_SESSIONS.get(token)
        if not session:
            await callback_query.answer("Session expired", show_alert=True)
            return
        if getattr(callback_query.from_user, "id", None) != session.get("user_id"):
            await callback_query.answer("Only requester can close this.", show_alert=True)
            return
        SEARCH_SESSIONS.pop(token, None)
        await callback_query.message.edit_text("<code>RESULTS REDACTED</code>")
        await callback_query.answer("Closed")
        return

    if not data.startswith("gds:"):
        await callback_query.answer()
        return

    parts = data.split(":")
    if len(parts) != 3:
        await callback_query.answer()
        return

    token, page_raw = parts[1], parts[2]
    session = SEARCH_SESSIONS.get(token)
    if not session:
        await callback_query.answer("Session expired", show_alert=True)
        return

    if getattr(callback_query.from_user, "id", None) != session.get("user_id"):
        await callback_query.answer("Only requester can change pages.", show_alert=True)
        return

    try:
        page = int(page_raw)
    except ValueError:
        await callback_query.answer()
        return

    text, total_pages, safe_page = _build_search_page_text(
        session["results"],
        session["query"],
        page,
    )
    markup = _build_search_page_markup(token, safe_page, total_pages)
    await callback_query.message.edit_text(
        text,
        reply_markup=markup,
        link_preview_options=_LINK_PREVIEW_OFF,
    )
    await callback_query.answer()



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


def _run_search(cfg, query: str, item_type: str):
    cloner = DriveCloner(cfg)
    return cloner.search(query=query, limit=50, item_type=item_type)


def _parse_search_args(text: str) -> tuple[str, str, Optional[str]]:
    try:
        args = shlex.split(text)
    except ValueError as exc:
        return "", "files", f"Invalid search query: {exc}"

    dir_count = args.count("--dir")
    all_count = args.count("--all")
    if dir_count > 1 or all_count > 1:
        return "", "files", "Use --dir/--all only once."
    if dir_count and all_count:
        return "", "files", "Use either --dir or --all, not both."

    item_type = "files"
    if dir_count or all_count:
        flag = "--dir" if dir_count else "--all"
        if args[-1] != flag:
            return "", "files", f"Use {flag} only at the end. Example: /s ubuntu {flag}"
        item_type = "folders" if flag == "--dir" else "all"
        args = args[:-1]

    return " ".join(args).strip(), item_type, None


def _format_search_results(query: str, results: list[dict]) -> str:
    safe_query = html.escape(query)
    if not results:
        return f"<u><b>SEARCH:</b></u>\n<code>{safe_query}</code>\n<code>NO RESULTS</code>"

    text, _total_pages, _page = _build_search_page_text(results, query, 0)
    return text


def _build_search_page_text(results: list[dict], query: str, page: int) -> tuple[str, int, int]:
    total_count = len(results)
    total_pages = max(1, (total_count + SEARCH_RESULTS_PER_PAGE - 1) // SEARCH_RESULTS_PER_PAGE)
    page = min(max(page, 0), total_pages - 1)
    start = page * SEARCH_RESULTS_PER_PAGE
    end = start + SEARCH_RESULTS_PER_PAGE
    page_items = results[start:end]

    safe_query = html.escape(query)
    lines = [
        "<u><b>SEARCH:</b></u>",
        f"<code>{safe_query}</code>",
        f"<b>Page {page + 1}/{total_pages} | Total: {total_count}</b>\n",
    ]
    for item in page_items:
        safe_name = html.escape(item.get("name", "Unnamed"))
        safe_url = html.escape(item.get("url", ""), quote=True)
        safe_size = html.escape(_search_item_size(item))
        if safe_url:
            lines.append(
                f"<code>{safe_name}</code> • <code>{safe_size}</code> • "
                f"<a href=\"{safe_url}\"><b>LINK</b></a>\n"
            )
        else:
            lines.append(f"<code>{safe_name}</code> • <code>{safe_size}</code>\n")
    return "\n".join(lines), total_pages, page


def _search_item_size(item: dict) -> str:
    if item.get("computed_size") is not None:
        try:
            return _fmt_bytes(int(item["computed_size"]))
        except (TypeError, ValueError):
            return "Unknown"

    raw_size = item.get("size")
    if raw_size is None:
        return "Unknown"
    try:
        return _fmt_bytes(int(raw_size))
    except (TypeError, ValueError):
        return "Unknown"


def _build_search_page_markup(token: str, page: int, total_pages: int) -> InlineKeyboardMarkup:
    nav_buttons = []
    if page > 0:
        nav_buttons.append(InlineKeyboardButton("PREV", callback_data=f"gds:{token}:{page - 1}"))
    nav_buttons.append(InlineKeyboardButton(f"{page + 1}/{total_pages}", callback_data="gdsnoop"))
    if page < total_pages - 1:
        nav_buttons.append(InlineKeyboardButton("NEXT", callback_data=f"gds:{token}:{page + 1}"))

    rows = [nav_buttons]
    rows.append([InlineKeyboardButton("CLOSE", callback_data=f"gdsc:{token}")])
    return InlineKeyboardMarkup(rows)



def _simple_error_text(raw: str) -> str:
    msg = (raw or "").strip()
    upper = msg.upper()
    if "FILE EXISTS" in upper or "ALREADY EXISTS" in upper:
        return "FILE EXISTS"
    if "NOT FOUND OR NOT SHARED" in upper or "NOT SHARED WITH THE ACTIVE ACCOUNT" in upper:
        return "FILE NOT FOUND OR NOT SHARED"
    if "FILE DOESN'T EXIST" in upper or "NOT FOUND" in upper:
        return "FILE DOESN'T EXIST"
    if "RATE LIMIT" in upper or "TOO MANY REQUESTS" in upper or "429" in upper:
        return "RATE LIMIT EXCEEDED"
    if "DAILY LIMIT" in upper:
        return "DAILY LIMIT EXCEEDED"
    if "STORAGE QUOTA" in upper or "QUOTA EXCEEDED" in upper:
        return "STORAGE QUOTA EXCEEDED"
    if "COPY IS RESTRICTED" in upper or "COPYING THIS FILE IS DISABLED" in upper:
        return "COPY RESTRICTED"
    if (
        "TEMPORARILY UNAVAILABLE" in upper
        or " 500" in upper
        or " 502" in upper
        or " 503" in upper
        or " 504" in upper
    ):
        return "DRIVE TEMPORARILY UNAVAILABLE"
    if "AUTH FAILED" in upper or "INVALID CREDENTIAL" in upper or " 401" in upper:
        return "GOOGLE AUTH FAILED"
    if "INVALID GOOGLE DRIVE LINK" in upper:
        return "INVALID DRIVE LINK"
    if "BAD REQUEST" in upper or " 400" in upper:
        return "BAD DRIVE REQUEST"
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
    app.on_message(filters.command(["s", "search"]))(search_command)
    app.on_message(filters.command("n"))(delete_command)
    app.on_callback_query(filters.regex(r"^(gds:|gdsc:|gdsnoop$)"))(search_pagination_callback)

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
