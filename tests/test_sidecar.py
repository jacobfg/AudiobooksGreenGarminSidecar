import json
from pathlib import Path

from fastapi.testclient import TestClient

from sidecar.app import create_app
from sidecar.cache import MediaCache
from sidecar.config import Settings, VirtualPlaylist


class FakeAbs:
    async def validate_token(self, token):
        if token != "Bearer valid":
            raise RuntimeError("bad token")

    async def playlist_items(self, playlist_id):
        books = {
            "library-a-adventure": [{"id": "book-a", "media": {"metadata": {"title": "A", "authors": [{"name": "One"}]}}}],
            "library-b-adventure": [
                {"id": "book-a", "media": {"metadata": {"title": "A", "authors": [{"name": "One"}]}}},
                {"id": "book-b", "media": {"metadata": {"title": "B", "authors": []}}},
            ],
        }
        return books[playlist_id]

    async def item(self, item_id):
        return {
            "id": item_id,
            "media": {
                "metadata": {"title": item_id.upper(), "authors": [{"name": "Test Author"}]},
                "audioFiles": [{"ino": f"file-{item_id}", "duration": 3661.8,
                                "metadata": {"filename": f"{item_id}.m4b"}}],
            },
        }

    async def progress(self, item_id):
        return {"currentTime": 65.9, "progress": 0.018} if item_id == "book-a" else None


def settings(tmp_path: Path) -> Settings:
    return Settings("http://abs", "service-token", tmp_path, 3600, 60, 30, "96k", (
        VirtualPlaylist("garmin-adventure", "Adventure", ("library-a-adventure", "library-b-adventure")),
    ))


def write_manifest(root: Path, item_id: str) -> None:
    book = root / item_id
    book.mkdir()
    (book / "0001.mp3").write_bytes(b"mp3")
    (book / "manifest.json").write_text(json.dumps({"files": [{"id": "chunk-001", "path": "0001.mp3", "filename": "0001.mp3", "duration": 12.5}]}))


def test_virtual_playlist_merges_source_playlists_and_serves_manifest(tmp_path):
    write_manifest(tmp_path, "book-a")
    write_manifest(tmp_path, "book-b")
    app = create_app(settings(tmp_path), FakeAbs())
    with TestClient(app) as client:
        headers = {"Authorization": "Bearer valid"}
        assert client.get("/audiobookshelf/playlists", headers=headers).json() == [{"id": "garmin-adventure", "name": "Adventure", "libraryId": "garmin-sidecar"}]
        books = client.get("/audiobookshelf/playlist", params={"playlist_id": "garmin-adventure"}, headers=headers).json()
        assert [book["id"] for book in books] == ["book-a", "book-b"]
        manifest = client.get("/audiobookshelf/book", params={"book_id": "book-a", "limit": 30}, headers=headers).json()
        assert manifest["total"] == 1
        assert manifest["files"][0]["id"] == "chunk-001"
        assert manifest["files"][0]["filename"] == "0001.mp3"
        response = client.get("/api/items/book-a/file/chunk-001/download", headers=headers)
        assert response.status_code == 200
        assert response.content == b"mp3"


def test_invalid_bearer_token_is_rejected(tmp_path):
    app = create_app(settings(tmp_path), FakeAbs())
    with TestClient(app) as client:
        assert client.get("/audiobookshelf/playlists").status_code == 401
        assert client.get("/audiobookshelf/playlists", headers={"Authorization": "Bearer no"}).status_code == 401


def test_playlist_status_refreshes_and_reports_source_progress_and_cache(tmp_path):
    write_manifest(tmp_path, "book-a")
    write_manifest(tmp_path, "book-b")
    app = create_app(settings(tmp_path), FakeAbs())
    with TestClient(app) as client:
        response = client.get("/garmin/playlist-status/adventure", headers={"Authorization": "Bearer valid"})
    assert response.status_code == 200
    payload = response.json()
    assert payload["playlist"]["id"] == "garmin-adventure"
    assert payload["book_count"] == 2
    first = payload["books"][0]
    assert first["duration"] == "1:01:01"
    assert first["position"] == "0:01:05"
    assert first["cache"]["state"] == "ready"
    assert first["source_files"][0]["filename"] == "book-a.m4b"


def test_prune_removes_only_unmanaged_book_caches(tmp_path):
    write_manifest(tmp_path, "keep")
    write_manifest(tmp_path, "remove")
    cache = MediaCache(settings(tmp_path), FakeAbs())
    cache.prune({"keep"})
    assert (tmp_path / "keep").is_dir()
    assert not (tmp_path / "remove").exists()


def test_cache_status_reports_exact_generation_file_progress(tmp_path):
    cache = MediaCache(settings(tmp_path), FakeAbs())
    cache.running.add("book-building")
    cache.generation["book-building"] = {
        "phase": "rendering", "current": 3, "total": 12, "files": "3/12",
    }
    status = cache.status("book-building")
    assert status["state"] == "generating"
    assert status["generation"]["files"] == "3/12"
