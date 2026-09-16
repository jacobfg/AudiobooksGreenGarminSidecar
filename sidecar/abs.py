"""Minimal ABS client used by the personal sidecar."""

import aiohttp
from typing import Optional


class AbsClient:
    def __init__(self, base_url: str, service_token: str):
        self.base_url = base_url.rstrip("/")
        self.service_token = service_token

    @staticmethod
    def headers(token: str) -> dict[str, str]:
        return {"Authorization": f"Bearer {token.removeprefix('Bearer ').strip()}"}

    async def _json(self, path: str, token: Optional[str] = None):
        async with aiohttp.ClientSession() as session:
            async with session.get(self.base_url + path, headers=self.headers(token or self.service_token)) as response:
                if not response.ok:
                    raise RuntimeError(f"ABS GET {path} failed ({response.status}): {await response.text()}")
                return await response.json()

    async def validate_token(self, token: str) -> None:
        await self._json("/api/me", token)

    async def playlist_items(self, playlist_id: str) -> list[dict]:
        payload = await self._json(f"/api/playlists/{playlist_id}")
        items = payload.get("items", []) if isinstance(payload, dict) else []
        result = []
        for item in items:
            book = item.get("libraryItem") if isinstance(item, dict) else None
            if isinstance(book, dict) and isinstance(book.get("id"), str):
                result.append(book)
        return result

    async def item(self, item_id: str) -> dict:
        payload = await self._json(f"/api/items/{item_id}")
        if not isinstance(payload, dict):
            raise RuntimeError(f"ABS item {item_id} did not return an object")
        return payload

    async def progress(self, item_id: str) -> Optional[dict]:
        """Return the configured ABS user's progress, if the book has any."""
        path = f"/api/me/progress/{item_id}"
        async with aiohttp.ClientSession() as session:
            async with session.get(self.base_url + path, headers=self.headers(self.service_token)) as response:
                if response.status == 404:
                    return None
                if not response.ok:
                    raise RuntimeError(f"ABS GET {path} failed ({response.status}): {await response.text()}")
                payload = await response.json()
        return payload if isinstance(payload, dict) else None

    async def download(self, item_id: str, file_id: str, target) -> None:
        path = f"/api/items/{item_id}/file/{file_id}/download"
        async with aiohttp.ClientSession() as session:
            async with session.get(self.base_url + path, headers=self.headers(self.service_token)) as response:
                if not response.ok:
                    raise RuntimeError(f"ABS download failed ({response.status}): {await response.text()}")
                with target.open("wb") as output:
                    async for chunk in response.content.iter_chunked(1024 * 1024):
                        output.write(chunk)


def book_summary(book: dict) -> dict:
    media = book.get("media") if isinstance(book, dict) else {}
    metadata = media.get("metadata") if isinstance(media, dict) else {}
    authors = metadata.get("authors") if isinstance(metadata, dict) else []
    author = authors[0].get("name", "") if isinstance(authors, list) and authors and isinstance(authors[0], dict) else ""
    return {"id": book["id"], "title": metadata.get("title", "") or "", "author": author or ""}
