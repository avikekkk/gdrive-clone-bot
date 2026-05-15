from __future__ import annotations

import json
import re
from dataclasses import dataclass
from typing import Callable, Optional

from google.auth.exceptions import RefreshError
from google.oauth2 import service_account
from google.oauth2.credentials import Credentials as UserCredentials
from google.auth.transport.requests import Request
from googleapiclient.discovery import build
from googleapiclient.errors import HttpError

from clonebot.config import BotConfig
from clonebot.progress import CloneProgress

DRIVE_FILE_MIME = "application/vnd.google-apps.file"
DRIVE_FOLDER_MIME = "application/vnd.google-apps.folder"

SCOPES = ["https://www.googleapis.com/auth/drive"]
SERVICE_ACCOUNT_AUTH_PREFIX = "service_account:"
OAUTH_AUTH_PREFIX = "oauth:"


class DriveCloneError(RuntimeError):
    """Raised for expected clone failures with user-friendly messages."""


class DriveDuplicateError(DriveCloneError):
    def __init__(self, existing: dict) -> None:
        super().__init__("FILE EXISTS")
        self.existing = existing


@dataclass(frozen=True)
class ParsedDriveLink:
    file_id: str
    resource_key: Optional[str]


FILE_ID_PATTERNS = [
    re.compile(r"/file/d/([a-zA-Z0-9_-]+)"),
    re.compile(r"/folders/([a-zA-Z0-9_-]+)"),
    re.compile(r"[?&]id=([a-zA-Z0-9_-]+)"),
]
RESOURCE_KEY_PATTERN = re.compile(r"[?&]resourcekey=([a-zA-Z0-9_-]+)", re.IGNORECASE)
RAW_ID_PATTERN = re.compile(r"^[a-zA-Z0-9_-]{10,}$")



def parse_drive_link(link: str) -> ParsedDriveLink:
    link = link.strip()
    if RAW_ID_PATTERN.match(link):
        return ParsedDriveLink(file_id=link, resource_key=None)
    for pattern in FILE_ID_PATTERNS:
        m = pattern.search(link)
        if m:
            file_id = m.group(1)
            rkm = RESOURCE_KEY_PATTERN.search(link)
            resource_key = rkm.group(1) if rkm else None
            return ParsedDriveLink(file_id=file_id, resource_key=resource_key)
    raise DriveCloneError("Invalid Google Drive link. Expected a file or folder URL.")


class DriveCloner:
    def __init__(self, config: BotConfig, preferred_auth: Optional[str] = None) -> None:
        self.config = config
        self._services = {}
        self._auth_order = self._available_auth_modes(config)
        if preferred_auth:
            if preferred_auth not in self._auth_order:
                raise DriveCloneError(f"Configured Google auth is missing: {preferred_auth}")
            self._auth_order = [preferred_auth]
        self.auth_mode = self._auth_order[0]
        self.service = None

    def _available_auth_modes(self, config: BotConfig) -> list[str]:
        modes = []
        for idx, _entry in enumerate(config.service_account_jsons, start=1):
            modes.append(f"{SERVICE_ACCOUNT_AUTH_PREFIX}{idx}")
        for idx, _entry in enumerate(config.google_oauth_credentials, start=1):
            modes.append(f"{OAUTH_AUTH_PREFIX}{idx}")
        return modes

    def _get_service(self, auth_mode: str):
        if auth_mode not in self._services:
            self._services[auth_mode] = self._build_service(self.config, auth_mode)
        return self._services[auth_mode]

    def _build_service(self, config: BotConfig, auth_mode: str):
        if auth_mode == "service_account" or auth_mode.startswith(SERVICE_ACCOUNT_AUTH_PREFIX):
            service_account_json = self._service_account_json_for_mode(config, auth_mode)
            creds = service_account.Credentials.from_service_account_info(
                eval_json(service_account_json), scopes=SCOPES
            )
        elif auth_mode == "oauth" or auth_mode.startswith(OAUTH_AUTH_PREFIX):
            oauth_credentials = self._oauth_credentials_for_mode(config, auth_mode)
            creds = UserCredentials(
                token=None,
                refresh_token=oauth_credentials.refresh_token,
                token_uri="https://oauth2.googleapis.com/token",
                client_id=oauth_credentials.client_id,
                client_secret=oauth_credentials.client_secret,
                scopes=SCOPES,
            )
            creds.refresh(Request())
        else:
            raise DriveCloneError(f"Unknown Google auth mode: {auth_mode}")

        return build("drive", "v3", credentials=creds, cache_discovery=False)

    def _service_account_json_for_mode(self, config: BotConfig, auth_mode: str) -> str:
        if auth_mode == "service_account":
            if not config.service_account_jsons:
                raise DriveCloneError("No service account is configured")
            return config.service_account_jsons[0]

        try:
            idx = int(auth_mode.removeprefix(SERVICE_ACCOUNT_AUTH_PREFIX)) - 1
        except ValueError as exc:
            raise DriveCloneError(f"Unknown Google auth mode: {auth_mode}") from exc

        if idx < 0 or idx >= len(config.service_account_jsons):
            raise DriveCloneError(f"Unknown Google auth mode: {auth_mode}")
        return config.service_account_jsons[idx]

    def _oauth_credentials_for_mode(self, config: BotConfig, auth_mode: str):
        if auth_mode == "oauth":
            if not config.google_oauth_credentials:
                raise DriveCloneError("No OAuth credentials are configured")
            return config.google_oauth_credentials[0]

        try:
            idx = int(auth_mode.removeprefix(OAUTH_AUTH_PREFIX)) - 1
        except ValueError as exc:
            raise DriveCloneError(f"Unknown Google auth mode: {auth_mode}") from exc

        if idx < 0 or idx >= len(config.google_oauth_credentials):
            raise DriveCloneError(f"Unknown Google auth mode: {auth_mode}")
        return config.google_oauth_credentials[idx]

    def _with_auth_fallback(self, operation: Callable[[], dict]) -> dict:
        last_exc = None
        for auth_mode in self._auth_order:
            self.auth_mode = auth_mode
            try:
                self.service = self._get_service(auth_mode)
                return operation()
            except DriveDuplicateError:
                raise
            except DriveCloneError as exc:
                if not self._should_try_next_auth(exc):
                    raise
                last_exc = exc
            except HttpError as exc:
                if not self._should_try_next_auth(exc):
                    raise
                last_exc = exc
            except RefreshError as exc:
                auth_exc = DriveCloneError("Google auth failed. Check the configured credentials.")
                if not self._should_try_next_auth(auth_exc):
                    raise auth_exc from exc
                last_exc = auth_exc
        if last_exc:
            raise last_exc
        raise DriveCloneError("No Google auth method is configured")

    def _should_try_next_auth(self, exc: Exception) -> bool:
        if self.auth_mode == self._auth_order[-1]:
            return False
        if isinstance(exc, HttpError):
            return getattr(exc.resp, "status", None) in (403, 404)
        if isinstance(exc, DriveCloneError):
            msg = str(exc).upper()
            return (
                "YOU DON'T HAVE PERMS" in msg
                or "FILE DOESN'T EXIST" in msg
                or "NOT FOUND" in msg
                or "GOOGLE AUTH FAILED" in msg
            )
        return False

    def _files_get(self, file_id: str, fields: str, resource_key: Optional[str] = None):
        kwargs = {
            "fileId": file_id,
            "supportsAllDrives": True,
            "fields": fields,
        }
        if resource_key:
            kwargs["resourceKey"] = resource_key
        return self.service.files().get(**kwargs).execute()

    def _files_get_with_fallback(self, file_id: str, fields: str, resource_key: Optional[str] = None):
        try:
            return self._files_get(file_id=file_id, fields=fields, resource_key=resource_key)
        except HttpError as exc:
            status = getattr(exc.resp, "status", None)
            # Some shared links carry unusable resource keys for this principal.
            # Retry once without resourceKey before returning not-found.
            if status == 404 and resource_key:
                return self._files_get(file_id=file_id, fields=fields, resource_key=None)
            raise

    def prepare_clone(self, source_link: str, destination_id: str) -> dict:
        return self._with_auth_fallback(lambda: self._prepare_clone(source_link, destination_id))

    def _prepare_clone(self, source_link: str, destination_id: str) -> dict:
        parsed = parse_drive_link(source_link)
        try:
            src_meta = self._files_get_with_fallback(
                parsed.file_id,
                "id,name,size,mimeType,webViewLink,resourceKey,trashed",
                resource_key=parsed.resource_key,
            )
        except HttpError as exc:
            if getattr(exc.resp, "status", None) == 404:
                raise DriveCloneError("FILE NOT FOUND OR NOT SHARED") from exc
            if getattr(exc.resp, "status", None) == 403:
                raise DriveCloneError("YOU DON'T HAVE PERMS") from exc
            raise normalize_http_error(exc) from exc

        if src_meta.get("trashed"):
            raise DriveCloneError("FILE DOESN'T EXIST")

        try:
            dst_meta = self._files_get(
                destination_id,
                "id,name,mimeType,trashed,capabilities(canAddChildren,canEdit)",
            )
        except HttpError as exc:
            if getattr(exc.resp, "status", None) in (403, 404):
                raise DriveCloneError("YOU DON'T HAVE PERMS") from exc
            raise normalize_http_error(exc) from exc

        if dst_meta.get("trashed"):
            raise DriveCloneError("YOU DON'T HAVE PERMS")
        if dst_meta.get("mimeType") != DRIVE_FOLDER_MIME:
            raise DriveCloneError("YOU DON'T HAVE PERMS")

        caps = dst_meta.get("capabilities", {})
        can_add = bool(caps.get("canAddChildren"))
        can_edit = bool(caps.get("canEdit"))
        if not can_add and not can_edit:
            raise DriveCloneError("YOU DON'T HAVE PERMS")

        try:
            duplicate = self._find_child_by_name(destination_id, src_meta.get("name", "Unnamed"))
        except HttpError as exc:
            raise normalize_http_error(exc) from exc
        if duplicate:
            duplicate["url"] = self._drive_url_for(duplicate)
            raise DriveDuplicateError(duplicate)

        return {
            "name": src_meta.get("name", "Unnamed"),
            "source_id": src_meta.get("id"),
            "source_mime_type": src_meta.get("mimeType"),
            "auth_mode": self.auth_mode,
        }

    def _files_copy(self, source_id: str, name: str, parent_id: str, resource_key: Optional[str] = None):
        kwargs = {
            "fileId": source_id,
            "supportsAllDrives": True,
            "body": {"name": name, "parents": [parent_id]},
            "fields": "id,name,size,webViewLink,mimeType",
        }
        if resource_key:
            kwargs["resourceKey"] = resource_key
        return self.service.files().copy(**kwargs).execute()

    def _create_folder(self, name: str, parent_id: str):
        body = {
            "name": name,
            "mimeType": DRIVE_FOLDER_MIME,
            "parents": [parent_id],
        }
        return self.service.files().create(
            body=body,
            supportsAllDrives=True,
            fields="id,name,webViewLink",
        ).execute()

    def _list_children(self, folder_id: str):
        children = []
        page_token = None
        while True:
            resp = self.service.files().list(
                q=f"'{folder_id}' in parents and trashed=false",
                fields="nextPageToken, files(id,name,size,mimeType,resourceKey)",
                supportsAllDrives=True,
                includeItemsFromAllDrives=True,
                pageToken=page_token,
                pageSize=1000,
            ).execute()
            children.extend(resp.get("files", []))
            page_token = resp.get("nextPageToken")
            if not page_token:
                break
        return children

    def _find_child_by_name(self, parent_id: str, name: str) -> Optional[dict]:
        escaped_name = name.replace("\\", "\\\\").replace("'", "\\'")
        resp = self.service.files().list(
            q=f"'{parent_id}' in parents and trashed=false and name='{escaped_name}'",
            fields="files(id,name,mimeType,webViewLink)",
            supportsAllDrives=True,
            includeItemsFromAllDrives=True,
            pageSize=1,
        ).execute()
        files = resp.get("files", [])
        return files[0] if files else None

    def search(self, query: str, limit: int = 10, item_type: str = "files") -> list[dict]:
        query = query.strip()
        if not query:
            raise DriveCloneError("Search query is required")
        if item_type not in ("files", "folders", "all"):
            raise DriveCloneError("Invalid search type")

        results = []
        seen_ids = set()
        last_exc = None
        per_auth_limit = max(limit, 1)

        for auth_mode in self._auth_order:
            self.auth_mode = auth_mode
            try:
                self.service = self._get_service(auth_mode)
                for item in self._search_all_shared_drives_with_active_service(
                    query,
                    per_auth_limit,
                    item_type,
                ):
                    item_id = item.get("id")
                    if not item_id or item_id in seen_ids:
                        continue
                    seen_ids.add(item_id)
                    self._attach_search_size(item)
                    item["url"] = self._drive_url_for(item)
                    item["auth_mode"] = auth_mode
                    results.append(item)
            except RefreshError as exc:
                last_exc = DriveCloneError("Google auth failed. Check the configured credentials.")
                continue
            except HttpError as exc:
                last_exc = normalize_http_error(exc)
                continue

        if results:
            return sorted(results, key=_search_sort_size, reverse=True)[:limit]
        if last_exc:
            raise last_exc
        return []

    def _search_all_shared_drives_with_active_service(
        self,
        query: str,
        limit: int,
        item_type: str,
    ) -> list[dict]:
        escaped_query = query.replace("\\", "\\\\").replace("'", "\\'")
        query_parts = [f"name contains '{escaped_query}'", "trashed=false"]
        if item_type == "files":
            query_parts.append(f"mimeType != '{DRIVE_FOLDER_MIME}'")
        elif item_type == "folders":
            query_parts.append(f"mimeType = '{DRIVE_FOLDER_MIME}'")

        list_kwargs = {
            "q": " and ".join(query_parts),
            "fields": "files(id,name,size,mimeType,webViewLink,modifiedTime,driveId,quotaBytesUsed)",
            "corpora": "allDrives",
            "supportsAllDrives": True,
            "includeItemsFromAllDrives": True,
            "pageSize": limit,
            "orderBy": "quotaBytesUsed desc",
        }

        try:
            resp = self.service.files().list(**list_kwargs).execute()
        except HttpError as exc:
            if getattr(exc.resp, "status", None) != 400:
                raise
            list_kwargs.pop("orderBy", None)
            resp = self.service.files().list(**list_kwargs).execute()

        return [item for item in resp.get("files", []) if item.get("driveId")]

    def _attach_search_size(self, item: dict) -> None:
        if item.get("size") is not None:
            item["computed_size"] = int(item.get("size") or 0)
            return
        if item.get("mimeType") != DRIVE_FOLDER_MIME:
            return

        try:
            total_bytes, total_files = self._scan_folder(item["id"])
        except HttpError:
            return

        item["computed_size"] = total_bytes
        item["computed_files"] = total_files

    def _drive_url_for(self, item: dict) -> str:
        if item.get("webViewLink"):
            return item["webViewLink"]
        if item.get("mimeType") == DRIVE_FOLDER_MIME:
            return f"https://drive.google.com/drive/folders/{item['id']}"
        return f"https://drive.google.com/file/d/{item['id']}/view"

    def _scan_folder(self, folder_id: str) -> tuple[int, int]:
        total_bytes = 0
        total_files = 0
        stack = [folder_id]
        while stack:
            current = stack.pop()
            for child in self._list_children(current):
                mime = child.get("mimeType")
                if mime == DRIVE_FOLDER_MIME:
                    stack.append(child["id"])
                    continue
                total_files += 1
                total_bytes += int(child.get("size", 0))
        return total_bytes, total_files

    def _copy_folder_recursive(
        self,
        source_folder_id: str,
        destination_folder_id: str,
        progress: CloneProgress,
        progress_cb: Callable[[CloneProgress, bool], None],
    ) -> None:
        for child in self._list_children(source_folder_id):
            mime = child.get("mimeType")
            if mime == DRIVE_FOLDER_MIME:
                new_folder = self._create_folder(child["name"], destination_folder_id)
                progress.status = f"Entering folder: {child['name']}"
                progress_cb(progress, False)
                self._copy_folder_recursive(child["id"], new_folder["id"], progress, progress_cb)
                continue

            progress.status = f"Copying file: {child['name']}"
            progress_cb(progress, False)
            self._files_copy(
                child["id"],
                child["name"],
                destination_folder_id,
                resource_key=child.get("resourceKey"),
            )
            progress.copied_files += 1
            progress.copied_bytes += int(child.get("size", 0))
            progress_cb(progress, False)

    def clone(
        self,
        source_link: str,
        destination_id: str,
        progress: CloneProgress,
        progress_cb: Callable[[CloneProgress, bool], None],
    ) -> dict:
        return self._with_auth_fallback(
            lambda: self._clone(source_link, destination_id, progress, progress_cb)
        )

    def _clone(
        self,
        source_link: str,
        destination_id: str,
        progress: CloneProgress,
        progress_cb: Callable[[CloneProgress, bool], None],
    ) -> dict:
        parsed = parse_drive_link(source_link)

        try:
            src_meta = self._files_get_with_fallback(
                parsed.file_id,
                "id,name,size,mimeType,webViewLink,resourceKey,trashed",
                resource_key=parsed.resource_key,
            )
        except HttpError as exc:
            raise normalize_http_error(exc) from exc

        if src_meta.get("trashed"):
            raise DriveCloneError("FILE DOESN'T EXIST")

        # Re-check destination at clone-time to avoid race conditions since prepare_clone.
        try:
            dst_meta = self._files_get(
                destination_id,
                "id,mimeType,trashed,capabilities(canAddChildren,canEdit)",
            )
        except HttpError as exc:
            if getattr(exc.resp, "status", None) in (403, 404):
                raise DriveCloneError("YOU DON'T HAVE PERMS") from exc
            raise normalize_http_error(exc) from exc
        if dst_meta.get("trashed") or dst_meta.get("mimeType") != DRIVE_FOLDER_MIME:
            raise DriveCloneError("YOU DON'T HAVE PERMS")
        caps = dst_meta.get("capabilities", {})
        if not bool(caps.get("canAddChildren")) and not bool(caps.get("canEdit")):
            raise DriveCloneError("YOU DON'T HAVE PERMS")

        try:
            duplicate = self._find_child_by_name(destination_id, src_meta.get("name", "Unnamed"))
        except HttpError as exc:
            raise normalize_http_error(exc) from exc
        if duplicate:
            duplicate["url"] = self._drive_url_for(duplicate)
            raise DriveDuplicateError(duplicate)

        progress.task_name = src_meta.get("name", "Unnamed")

        try:
            if src_meta.get("mimeType") == DRIVE_FOLDER_MIME:
                progress.status = "Scanning folder for total size"
                progress_cb(progress, True)
                total_bytes, total_files = self._scan_folder(src_meta["id"])
                progress.total_bytes = total_bytes
                progress.total_files = total_files

                top_folder = self._create_folder(src_meta["name"], destination_id)
                progress.status = "Starting server-side folder copy"
                progress_cb(progress, True)

                self._copy_folder_recursive(src_meta["id"], top_folder["id"], progress, progress_cb)

                progress.status = "Finalizing"
                progress_cb(progress, True)
                return {
                    "id": top_folder["id"],
                    "name": top_folder["name"],
                    "mime_type": DRIVE_FOLDER_MIME,
                    "url": top_folder.get("webViewLink")
                    or f"https://drive.google.com/drive/folders/{top_folder['id']}",
                }

            size = int(src_meta.get("size", 0))
            progress.total_bytes = size
            progress.total_files = 1
            progress.status = f"Copying file: {src_meta['name']}"
            progress_cb(progress, True)

            copied = self._files_copy(
                src_meta["id"],
                src_meta["name"],
                destination_id,
                resource_key=src_meta.get("resourceKey") or parsed.resource_key,
            )
            progress.copied_files = 1
            progress.copied_bytes = size
            progress.status = "Finalizing"
            progress_cb(progress, True)

            return {
                "id": copied["id"],
                "name": copied["name"],
                "mime_type": copied.get("mimeType", "unknown"),
                "url": copied.get("webViewLink")
                or f"https://drive.google.com/file/d/{copied['id']}/view",
            }
        except HttpError as exc:
            raise normalize_http_error(exc) from exc

    def delete(self, source_link: str) -> dict:
        target = self.prepare_delete(source_link)
        return self.perform_delete(target)

    def prepare_delete(self, source_link: str) -> dict:
        return self._with_auth_fallback(lambda: self._prepare_delete(source_link))

    def _prepare_delete(self, source_link: str) -> dict:
        parsed = parse_drive_link(source_link)
        try:
            src_meta = self._files_get_with_fallback(
                parsed.file_id,
                "id,name,mimeType,webViewLink,capabilities(canDelete,canTrash),trashed",
                resource_key=parsed.resource_key,
            )
            if src_meta.get("trashed"):
                raise DriveCloneError("FILE DOESN'T EXIST")

            capabilities = src_meta.get("capabilities", {})
            can_delete = bool(capabilities.get("canDelete"))
            can_trash = bool(capabilities.get("canTrash"))

            if can_delete:
                mode = "delete"
            elif can_trash:
                mode = "trash"
            else:
                raise DriveCloneError("YOU DON'T HAVE PERMS")
        except HttpError as exc:
            if getattr(exc.resp, "status", None) == 404:
                raise DriveCloneError("FILE NOT FOUND OR NOT SHARED") from exc
            raise normalize_http_error(exc) from exc

        return {
            "id": src_meta["id"],
            "name": src_meta.get("name", "Unnamed"),
            "mimeType": src_meta.get("mimeType"),
            "mode": mode,
            "auth_mode": self.auth_mode,
        }

    def perform_delete(self, target: dict) -> dict:
        try:
            self.service = self._get_service(self.auth_mode)
            if target["mode"] == "delete":
                self.service.files().delete(
                    fileId=target["id"],
                    supportsAllDrives=True,
                ).execute()
                action = "deleted"
            else:
                self.service.files().update(
                    fileId=target["id"],
                    supportsAllDrives=True,
                    body={"trashed": True},
                    fields="id,trashed",
                ).execute()
                action = "trashed"
        except HttpError as exc:
            if getattr(exc.resp, "status", None) == 404:
                raise DriveCloneError("FILE NOT FOUND OR NOT SHARED") from exc
            if getattr(exc.resp, "status", None) == 403:
                raise DriveCloneError("YOU DON'T HAVE PERMS") from exc
            raise normalize_http_error(exc) from exc
        except RefreshError as exc:
            raise DriveCloneError("Google auth failed. Check the configured credentials.") from exc

        item_type = "folder" if target.get("mimeType") == DRIVE_FOLDER_MIME else "file"
        return {
            "id": target["id"],
            "name": target.get("name", "Unnamed"),
            "type": item_type,
            "action": action,
        }


def eval_json(json_string: str) -> dict:
    if json_string.strip().startswith("{"):
        return json.loads(json_string)

    with open(json_string, "r", encoding="utf-8") as f:
        return json.load(f)


def _search_sort_size(item: dict) -> int:
    try:
        return int(item.get("computed_size") or item.get("size") or item.get("quotaBytesUsed") or 0)
    except (TypeError, ValueError):
        return 0


def normalize_http_error(exc: HttpError) -> DriveCloneError:
    status = getattr(exc.resp, "status", None)
    message = "Google Drive API error"

    if hasattr(exc, "_get_reason"):
        reason = exc._get_reason()
        if reason:
            message = reason

    lower = message.lower()

    if status == 401:
        return DriveCloneError("Google auth failed. Check the configured credentials.")
    if status == 404:
        return DriveCloneError("Source item was not found or is not shared with the active account.")
    if status == 429:
        return DriveCloneError("Rate limit exceeded. Wait a bit and retry.")
    if status == 403:
        if (
            "ratelimitexceeded" in lower
            or "userratelimitexceeded" in lower
            or "sharingratelimitexceeded" in lower
            or "too many requests" in lower
            or "queries per minute" in lower
        ):
            return DriveCloneError("Rate limit exceeded. Wait a bit and retry.")
        if "dailylimitexceeded" in lower or "daily limit" in lower:
            return DriveCloneError("Drive API daily limit exceeded. Retry after the quota resets.")
        if "storagequotaexceeded" in lower or ("storage" in lower and "quota" in lower):
            return DriveCloneError("Destination storage quota exceeded. Free space and retry.")
        if "cannotcopyfile" in lower or "copying this file is disabled" in lower:
            return DriveCloneError("Copy is restricted for this file.")
        return DriveCloneError(
            "Permission denied (403). For private links, share the source with your service account(s)/OAuth user and ensure destination write access."
        )
    if status == 400:
        return DriveCloneError(f"Bad request to Drive API: {message}")
    if status in (500, 502, 503, 504):
        return DriveCloneError("Google Drive is temporarily unavailable. Retry later.")

    return DriveCloneError(f"Drive API error ({status or 'unknown'}): {message}")
