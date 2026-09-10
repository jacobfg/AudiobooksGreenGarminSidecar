"""Watch-facing virtual-playlist and Garmin media service."""

import asyncio
from contextlib import asynccontextmanager, suppress
import hashlib
import time
from typing import Optional

from fastapi import Depends, FastAPI, Header, HTTPException, Query
from fastapi.responses import FileResponse

from .abs import AbsClient, book_summary
from .cache import MediaCache
from .config import Settings, load_settings


def create_app(settings: Optional[Settings] = None, abs_client: Optional[AbsClient] = None) -> FastAPI:
    settings = settings or load_settings()
    abs_client = abs_client or AbsClient(settings.abs_url, settings.abs_token)
    cache = MediaCache(settings, abs_client)
    virtual_books: dict[str, list[dict]] = {}
    token_cache: dict[str, float] = {}

    async def reconcile() -> None:
        """Refresh all groups, queue missing media, then safely prune stale cache."""
        nonlocal virtual_books
        refreshed: dict[str, list[dict]] = {}
        all_ids: set[str] = set()
        for group in settings.playlists:
            merged: dict[str, dict] = {}
            for source_id in group.source_playlist_ids:
                for book in await abs_client.playlist_items(source_id):
                    merged.setdefault(book["id"], book)
            refreshed[group.id] = list(merged.values())
            all_ids.update(merged)
        # Only update/prune after every configured source has succeeded.
        virtual_books = refreshed
        for item_id in all_ids:
            cache.queue_item(item_id)
        cache.prune(all_ids)

    async def poller() -> None:
        while True:
            try:
                await reconcile()
            except Exception:
                import logging
                logging.getLogger(__name__).exception("ABS playlist reconciliation failed")
            await asyncio.sleep(settings.poll_interval_seconds)

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        cache.start()
        worker = asyncio.create_task(cache.worker())
        refresh = asyncio.create_task(poller())
        app.state.cache, app.state.reconcile = cache, reconcile
        try:
            yield
        finally:
            for task in (worker, refresh):
                task.cancel()
            for task in (worker, refresh):
                with suppress(asyncio.CancelledError):
                    await task

    app = FastAPI(title="AudiobooksGreen Garmin Sidecar", docs_url=None, redoc_url=None, lifespan=lifespan)

    async def require_abs_token(authorization: Optional[str] = Header(None, alias="Authorization")) -> None:
        if not authorization or not authorization.startswith("Bearer "):
            raise HTTPException(status_code=401, detail="Bearer token required")
        key = hashlib.sha256(authorization.encode()).hexdigest()
        now = time.monotonic()
        if token_cache.get(key, 0) > now:
            return
        try:
            await abs_client.validate_token(authorization)
        except Exception:
            raise HTTPException(status_code=401, detail="Invalid Audiobookshelf token") from None
        token_cache[key] = now + settings.auth_cache_seconds

    @app.get("/audiobookshelf/playlists", dependencies=[Depends(require_abs_token)])
    async def playlists():
        # This is deliberately a list, matching the generic proxy's compact API.
        return [{"id": group.id, "name": group.name, "libraryId": "garmin-sidecar"} for group in settings.playlists]

    @app.get("/audiobookshelf/playlist", dependencies=[Depends(require_abs_token)])
    async def playlist(playlist_id: str, server: str = Query("")):
        del server
        if settings.playlist(playlist_id) is None:
            raise HTTPException(status_code=404, detail="Unknown virtual playlist")
        # Before the first poll, synchronously populate this one group.
        if playlist_id not in virtual_books:
            await reconcile()
        return [book_summary(book) for book in virtual_books.get(playlist_id, [])]

    @app.get("/audiobookshelf/book", dependencies=[Depends(require_abs_token)])
    async def book(book_id: str, server: str = Query(""), skip: int = 0, limit: int = 0):
        del server
        if not any(book_id == item["id"] for books in virtual_books.values() for item in books):
            return {"files": [], "skip": skip, "limit": limit, "total": 0}
        manifest = cache.manifest(book_id)
        if manifest is None:
            cache.queue_item(book_id)
            return {"files": [], "skip": skip, "limit": limit, "total": 0}
        files = manifest["files"]
        selected = files[skip:] if skip >= 0 else files
        if limit > 0:
            selected = selected[:limit]
        return {"files": [{"id": item["id"], "filename": item["filename"], "duration": item["duration"]} for item in selected], "skip": skip, "limit": limit, "total": len(files)}

    @app.get("/api/items/{book_id}/file/{file_id}/download", dependencies=[Depends(require_abs_token)])
    async def download(book_id: str, file_id: str):
        path = cache.file(book_id, file_id)
        if path is None:
            raise HTTPException(status_code=404, detail="Cached Garmin media not found")
        return FileResponse(path, media_type="audio/mpeg", filename=path.name)

    @app.get("/healthz")
    async def healthz():
        return {"status": "ok"}

    return app
