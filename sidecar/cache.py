"""Safe cache publication and single-worker Garmin MP3 conversion."""

import asyncio
import json
from pathlib import Path
import re
import shutil
import subprocess
import uuid
from typing import Optional

from .abs import AbsClient
from .config import Settings

SAFE_ID = re.compile(r"^[A-Za-z0-9_-]+$")
SUPPORTED_EXTENSIONS = {".m4b", ".mp3"}


class MediaCache:
    def __init__(self, settings: Settings, abs_client: AbsClient):
        self.settings, self.abs = settings, abs_client
        self.root = settings.cache_dir.resolve()
        self.root.mkdir(parents=True, exist_ok=True)
        (self.root / ".staging").mkdir(exist_ok=True)
        self.queue = None
        self.pending: set[str] = set()
        self.running: set[str] = set()
        self.errors: dict[str, str] = {}

    def start(self) -> None:
        """Bind the queue to the ASGI application's running event loop."""
        self.queue = asyncio.Queue()

    def _directory(self, item_id: str) -> Optional[Path]:
        if not SAFE_ID.fullmatch(item_id):
            return None
        candidate = (self.root / item_id).resolve()
        return candidate if candidate.parent == self.root else None

    def manifest(self, item_id: str) -> Optional[dict]:
        directory = self._directory(item_id)
        if directory is None:
            return None
        try:
            data = json.loads((directory / "manifest.json").read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return None
        files = data.get("files") if isinstance(data, dict) else None
        if not isinstance(files, list) or not files:
            return None
        seen = set()
        for entry in files:
            if not isinstance(entry, dict):
                return None
            file_id, path, filename, duration = entry.get("id"), entry.get("path"), entry.get("filename"), entry.get("duration")
            if (not isinstance(file_id, str) or not SAFE_ID.fullmatch(file_id) or file_id in seen
                    or not isinstance(path, str) or not isinstance(filename, str)
                    or isinstance(duration, bool) or not isinstance(duration, (int, float)) or duration < 0):
                return None
            resolved = (directory / path).resolve()
            if resolved.parent != directory or not resolved.is_file():
                return None
            seen.add(file_id)
        return data

    def file(self, item_id: str, file_id: str) -> Optional[Path]:
        manifest = self.manifest(item_id)
        directory = self._directory(item_id)
        if manifest is None or directory is None:
            return None
        for entry in manifest["files"]:
            if entry["id"] == file_id:
                return directory / entry["path"]
        return None

    def queue_item(self, item_id: str) -> None:
        if self.manifest(item_id) is not None or item_id in self.pending or self._directory(item_id) is None:
            return
        self.pending.add(item_id)
        if self.queue is None:
            raise RuntimeError("media cache worker has not been started")
        self.queue.put_nowait(item_id)

    async def worker(self) -> None:
        if self.queue is None:
            raise RuntimeError("media cache worker has not been started")
        while True:
            item_id = await self.queue.get()
            self.running.add(item_id)
            try:
                await self.build(item_id)
                self.errors.pop(item_id, None)
            except Exception as error:
                self.errors[item_id] = str(error)
                # Retain staging output for diagnosis; a later poll retries it.
                import logging
                logging.getLogger(__name__).exception("Garmin conversion failed for %s", item_id)
            finally:
                self.running.discard(item_id)
                self.pending.discard(item_id)
                self.queue.task_done()

    def status(self, item_id: str) -> dict:
        """Describe cache availability and the current background job state."""
        manifest = self.manifest(item_id)
        if manifest is not None:
            files = manifest["files"]
            return {
                "state": "ready",
                "exists": True,
                "file_count": len(files),
                "duration_seconds": sum(entry["duration"] for entry in files),
            }
        if item_id in self.running:
            state = "generating"
        elif item_id in self.pending:
            state = "queued"
        elif item_id in self.errors:
            state = "failed"
        else:
            state = "missing"
        return {"state": state, "exists": False, "file_count": 0, "duration_seconds": 0.0,
                "last_error": self.errors.get(item_id)}

    async def build(self, item_id: str) -> None:
        directory = self._directory(item_id)
        if directory is None or self.manifest(item_id) is not None:
            return
        stage = self.root / ".staging" / f"{item_id}-{uuid.uuid4().hex}"
        sources, rendered = stage / "sources", stage / "rendered"
        sources.mkdir(parents=True)
        item = await self.abs.item(item_id)
        audio_files = self._audio_files(item)
        downloaded = []
        for index, info in enumerate(audio_files, 1):
            filename = info["metadata"]["filename"]
            target = sources / f"source-{index:04d}{Path(filename).suffix.lower()}"
            await self.abs.download(item_id, str(info["ino"]), target)
            downloaded.append(target)
        await asyncio.to_thread(self._render, stage, rendered, downloaded, audio_files)
        manifest = await asyncio.to_thread(self._manifest, rendered)
        (rendered / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        if self.manifest_from(rendered) is None:
            raise RuntimeError("generated media manifest is invalid")
        await asyncio.to_thread(self._publish, directory, rendered, stage)

    @staticmethod
    def _audio_files(item: dict) -> list[dict]:
        media = item.get("media") if isinstance(item, dict) else None
        files = media.get("audioFiles") if isinstance(media, dict) else None
        if not isinstance(files, list) or not files:
            raise RuntimeError("ABS item has no audio files")
        for entry in files:
            metadata = entry.get("metadata") if isinstance(entry, dict) else None
            name = metadata.get("filename") if isinstance(metadata, dict) else None
            if not name or entry.get("ino") is None or Path(name).suffix.lower() not in SUPPORTED_EXTENSIONS:
                raise RuntimeError("only M4B and MP3 source media are supported")
        return files

    def _render(self, stage: Path, rendered: Path, sources: list[Path], audio_files: list[dict]) -> None:
        rendered.mkdir()
        output_index = 1
        for source, source_info in zip(sources, audio_files):
            units = self._chapters(source)
            if len(sources) > 1:
                units = [(0.0, self._duration(source), Path(source_info["metadata"]["filename"]).stem)]
            for start, duration, title in units:
                position = start
                end = start + duration
                while position < end:
                    part = min(self.settings.chunk_minutes * 60, end - position)
                    # Keep the cache layout deliberately simple and stable.
                    # Chapter titles remain available from ABS metadata; Garmin
                    # only needs the chunk order and manifest duration here.
                    target = rendered / f"{output_index:04d}.mp3"
                    self._run(["ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-ss", f"{position:.3f}", "-i", str(source), "-t", f"{part:.3f}", "-vn", "-map_metadata", "-1", "-map_chapters", "-1", "-ar", "44100", "-ac", "2", "-c:a", "libmp3lame", "-b:a", self.settings.bitrate, str(target)], stage)
                    output_index, position = output_index + 1, position + part
        if output_index == 1:
            raise RuntimeError("conversion produced no files")

    @staticmethod
    def _safe_name(value: str) -> str:
        return re.sub(r"\s+", " ", re.sub(r"[^A-Za-z0-9._ -]", "", value)).strip(". ")

    @staticmethod
    def _run(command: list[str], cwd: Path) -> None:
        result = subprocess.run(command, cwd=cwd, capture_output=True, text=True, check=False)
        if result.returncode:
            raise RuntimeError(result.stderr.strip() or "ffmpeg failed")

    @staticmethod
    def _duration(path: Path) -> float:
        result = subprocess.run(["ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", str(path)], capture_output=True, text=True, check=False)
        if result.returncode:
            raise RuntimeError(result.stderr.strip() or "ffprobe failed")
        return float(result.stdout.strip())

    def _chapters(self, path: Path) -> list[tuple[float, float, str]]:
        result = subprocess.run(["ffprobe", "-v", "error", "-show_chapters", "-of", "json", str(path)], capture_output=True, text=True, check=False)
        if result.returncode:
            raise RuntimeError(result.stderr.strip() or "ffprobe chapter scan failed")
        chapters = json.loads(result.stdout).get("chapters", [])
        parsed = []
        for chapter in chapters:
            try:
                start, end = float(chapter["start_time"]), float(chapter["end_time"])
                if end > start:
                    parsed.append((start, end - start, chapter.get("tags", {}).get("title", "Chapter")))
            except (KeyError, TypeError, ValueError):
                continue
        return parsed or [(0.0, self._duration(path), path.stem)]

    @staticmethod
    def _manifest(rendered: Path) -> dict:
        files = []
        for index, path in enumerate(sorted(rendered.glob("*.mp3")), 1):
            duration = MediaCache._duration(path)
            files.append({"id": f"chunk-{index:03d}", "path": path.name, "filename": path.name, "duration": duration})
        return {"files": files}

    @staticmethod
    def manifest_from(directory: Path) -> Optional[dict]:
        try:
            data = json.loads((directory / "manifest.json").read_text())
            files = data.get("files")
            if not isinstance(files, list) or not files:
                return None
            if any(not (directory / entry.get("path", "")).is_file() for entry in files if isinstance(entry, dict)):
                return None
            return data
        except (OSError, json.JSONDecodeError, AttributeError):
            return None

    @staticmethod
    def _publish(directory: Path, rendered: Path, stage: Path) -> None:
        temporary = directory.with_name(directory.name + ".new")
        shutil.rmtree(temporary, ignore_errors=True)
        shutil.move(str(rendered), temporary)
        shutil.rmtree(directory, ignore_errors=True)
        temporary.replace(directory)
        shutil.rmtree(stage, ignore_errors=True)

    def prune(self, keep_ids: set[str]) -> None:
        for entry in self.root.iterdir():
            if entry.name == ".staging" or not entry.is_dir() or not SAFE_ID.fullmatch(entry.name):
                continue
            if entry.name not in keep_ids:
                shutil.rmtree(entry)
