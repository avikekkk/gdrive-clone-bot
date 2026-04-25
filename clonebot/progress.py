from __future__ import annotations

import html
import math
import time
from dataclasses import dataclass, field
from typing import Optional


def _fmt_bytes(num_bytes: float) -> str:
    units = ["B", "KB", "MB", "GB", "TB"]
    value = float(max(num_bytes, 0.0))
    for unit in units:
        if value < 1024.0 or unit == units[-1]:
            if unit == "B":
                return f"{int(value)} {unit}"
            return f"{value:.2f} {unit}"
        value /= 1024.0
    return f"{value:.2f} TB"


def _fmt_duration(seconds: float) -> str:
    if not math.isfinite(seconds) or seconds < 0:
        return "--"
    sec = int(seconds)
    h, rem = divmod(sec, 3600)
    m, s = divmod(rem, 60)
    if h > 0:
        return f"{h:02d}:{m:02d}:{s:02d}"
    return f"{m:02d}:{s:02d}"


def _clip(text: str, max_len: int = 80) -> str:
    text = (text or "").strip().replace("\n", " ")
    if len(text) <= max_len:
        return text
    return text[: max_len - 3] + "..."


@dataclass
class CloneProgress:
    task_name: str
    total_bytes: int = 0
    total_files: int = 0
    copied_bytes: int = 0
    copied_files: int = 0
    started_at: float = field(default_factory=time.time)
    status: str = "Initializing"

    @property
    def percentage(self) -> float:
        if self.total_bytes > 0:
            return min(100.0, (self.copied_bytes / self.total_bytes) * 100.0)
        if self.total_files > 0:
            return min(100.0, (self.copied_files / self.total_files) * 100.0)
        return 0.0

    @property
    def elapsed(self) -> float:
        return max(0.001, time.time() - self.started_at)

    @property
    def speed_bps(self) -> float:
        return self.copied_bytes / self.elapsed if self.copied_bytes > 0 else 0.0

    @property
    def eta_seconds(self) -> float:
        if self.total_bytes <= 0:
            return float("nan")
        remaining = max(0, self.total_bytes - self.copied_bytes)
        speed = self.speed_bps
        if speed <= 0:
            return float("nan")
        return remaining / speed

    def progress_bar(self, width: int = 12) -> str:
        pct = max(0.0, min(100.0, self.percentage))
        filled = int((pct / 100.0) * width)
        return "[" + ("▓" * filled) + ("░" * (width - filled)) + "]"

    def as_message(self) -> str:
        safe_name = html.escape(_clip(self.task_name, 90))
        total_size = _fmt_bytes(self.total_bytes) if self.total_bytes else "Unknown"
        safe_bar = html.escape(self.progress_bar())
        safe_done = html.escape(_fmt_bytes(self.copied_bytes))
        safe_total = html.escape(total_size)
        safe_speed = html.escape(f"{_fmt_bytes(self.speed_bps)}/s")
        safe_eta = html.escape(_fmt_duration(self.eta_seconds))

        return (
            "<u><b>CLONING</b></u>\n"
            f"<code>{safe_name}</code>\n"
            f"<code>{safe_bar} {self.percentage:.2f}%</code>\n"
            f"<code>{safe_done}</code> <code>/</code> <code>{safe_total}</code>\n"
            f"<code>↓{safe_speed} | ETA : {safe_eta}</code>"
        )

    def completion_message(self, output_name: str, output_url: str, mime_type: str) -> str:
        safe_name = html.escape(output_name)
        safe_size = html.escape(_fmt_bytes(self.copied_bytes))
        safe_url = html.escape(output_url, quote=True)
        return (
            "<code>CLONED:</code>\n"
            f"<code>{safe_name}</code>\n"
            f"<code>{safe_size} • </code>"
            f"<a href=\"{safe_url}\"><b>DL</b></a>"
        )


class ThrottledReporter:
    def __init__(self, min_interval_seconds: float = 2.5) -> None:
        self.min_interval_seconds = min_interval_seconds
        self._last_update_at = 0.0

    def should_update(self, force: bool = False) -> bool:
        now = time.time()
        if force or now - self._last_update_at >= self.min_interval_seconds:
            self._last_update_at = now
            return True
        return False
