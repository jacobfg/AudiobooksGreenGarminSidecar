"""Configuration for Garmin-specific virtual playlists."""

from dataclasses import dataclass
import os
from pathlib import Path
import re
from typing import Optional

import yaml

SAFE_ID = re.compile(r"^[A-Za-z0-9_-]+$")


@dataclass(frozen=True)
class VirtualPlaylist:
    id: str
    name: str
    source_playlist_ids: tuple[str, ...]


@dataclass(frozen=True)
class Settings:
    abs_url: str
    abs_token: str
    cache_dir: Path
    poll_interval_seconds: int
    auth_cache_seconds: int
    chunk_minutes: int
    bitrate: str
    playlists: tuple[VirtualPlaylist, ...]

    def playlist(self, playlist_id: str) -> Optional[VirtualPlaylist]:
        return next((entry for entry in self.playlists if entry.id == playlist_id), None)


def _positive_int(name: str, default: int) -> int:
    value = int(os.getenv(name, str(default)))
    if value < 1:
        raise ValueError(f"{name} must be positive")
    return value


def load_settings(path: Optional[str] = None) -> Settings:
    config_path = Path(path or os.getenv("SIDECAR_CONFIG", "/app/config.yaml"))
    with config_path.open(encoding="utf-8") as source:
        data = yaml.safe_load(source) or {}
    raw_playlists = data.get("virtual_playlists")
    if not isinstance(raw_playlists, list) or not raw_playlists:
        raise ValueError("config must contain a non-empty virtual_playlists list")

    playlists = []
    seen = set()
    for entry in raw_playlists:
        if not isinstance(entry, dict):
            raise ValueError("each virtual playlist must be a mapping")
        playlist_id, name, source_ids = entry.get("id"), entry.get("name"), entry.get("source_playlist_ids")
        if not isinstance(playlist_id, str) or not SAFE_ID.fullmatch(playlist_id) or playlist_id in seen:
            raise ValueError("virtual playlist IDs must be unique letters, digits, _ or -")
        if not isinstance(name, str) or not name.strip() or not isinstance(source_ids, list) or not source_ids:
            raise ValueError(f"virtual playlist {playlist_id} requires name and source_playlist_ids")
        if any(not isinstance(source_id, str) or not source_id.strip()
               or any(not part.strip() for part in source_id.split("/", 1)) for source_id in source_ids):
            raise ValueError(f"virtual playlist {playlist_id} has an invalid source playlist ID or library/playlist name")
        seen.add(playlist_id)
        playlists.append(VirtualPlaylist(playlist_id, name.strip(), tuple(source.strip() for source in source_ids)))

    abs_url, abs_token = os.getenv("ABS_URL", "").rstrip("/"), os.getenv("ABS_TOKEN", "")
    abs_token_file = os.getenv("ABS_TOKEN_FILE", "")
    if abs_token and abs_token_file:
        raise ValueError("Set only one of ABS_TOKEN or ABS_TOKEN_FILE")
    if abs_token_file:
        try:
            abs_token = Path(abs_token_file).read_text(encoding="utf-8").strip()
        except (OSError, UnicodeError) as exc:
            raise ValueError("ABS_TOKEN_FILE must point to a readable UTF-8 token file") from exc
    if not abs_url or not abs_token:
        raise ValueError("ABS_URL and either ABS_TOKEN or a non-empty ABS_TOKEN_FILE are required")
    return Settings(abs_url, abs_token, Path(os.getenv("CACHE_DIR", "/app/cache")),
                    _positive_int("POLL_INTERVAL_SECONDS", 300), _positive_int("AUTH_CACHE_SECONDS", 300),
                    _positive_int("CACHE_CHUNK_MINUTES", 30), os.getenv("CACHE_BITRATE", "96k"), tuple(playlists))
