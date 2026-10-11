from dataclasses import replace
from unittest.mock import AsyncMock

import pytest
import yaml
from fastapi.testclient import TestClient

from sidecar.abs import AbsClient
from sidecar.app import create_app
from sidecar.config import VirtualPlaylist, load_settings
from test_sidecar import FakeAbs, settings, write_manifest


class NamedAbs(FakeAbs, AbsClient):
    def __init__(self):
        super().__init__("http://abs", "service-token")
        self.libraries = [{"id": "a", "name": "Library A"}, {"id": "b", "name": "Library B"}]
        self.playlists = []
        self.failure = False

    async def _json(self, path, **kwargs):
        if self.failure:
            raise RuntimeError("ABS unavailable")
        if path == "/api/libraries":
            return {"libraries": self.libraries}
        assert path == "/api/playlists"
        return {"playlists": self.playlists}


@pytest.mark.asyncio
async def test_names_match_case_whitespace_and_correct_library():
    client = NamedAbs()
    client.playlists = [
        {"id": "first", "libraryId": "a", "name": "Adventure Books"},
        {"id": "second", "libraryId": "b", "name": "ADVENTURE  books"},
    ]
    sources = {" library a / adventure   BOOKS ", "LIBRARY B/Adventure Books", "Missing/Adventure Books", "Library A/Missing"}
    assert await client.resolve_playlist_names(sources) == {
        " library a / adventure   BOOKS ": "first", "LIBRARY B/Adventure Books": "second",
        "Missing/Adventure Books": None, "Library A/Missing": None,
    }


@pytest.mark.asyncio
@pytest.mark.parametrize("duplicate_library", [False, True])
async def test_ambiguous_names_fail_instead_of_selecting_arbitrarily(duplicate_library):
    client = NamedAbs()
    client.playlists = [{"id": "first", "libraryId": "a", "name": "Adventure"}]
    if duplicate_library:
        client.libraries.append({"id": "duplicate", "name": " library  A "})
    else:
        client.playlists.append({"id": "duplicate", "libraryId": "a", "name": " adventure "})
    with pytest.raises(RuntimeError, match="Ambiguous"):
        await client.resolve_playlist_names({"Library A/Adventure"})


@pytest.mark.asyncio
async def test_invalid_listing_does_not_become_empty_sources():
    client = NamedAbs()
    client.playlists = None
    with pytest.raises(RuntimeError, match="invalid"):
        await client.resolve_playlist_names({"Library A/Adventure"})


def test_missing_then_recreated_playlist_refreshes_and_merges_ids(tmp_path, monkeypatch, caplog):
    # Avoid conversion work while testing discovery and cache pruning.
    monkeypatch.setattr("sidecar.cache.MediaCache.build", AsyncMock())
    client_abs = NamedAbs()
    config = replace(settings(tmp_path), playlists=(
        VirtualPlaylist("garmin-adventure", "Adventure", ("Library A/Adventure", "library-a-adventure")),
    ))
    write_manifest(tmp_path, "book-a")
    app = create_app(config, client_abs)
    with TestClient(app) as client:
        def refresh():
            response = client.get("/garmin/playlist-status/Adventure", headers={"Authorization": "Bearer valid"})
            assert response.status_code == 200
            return [book["id"] for book in response.json()["books"]]

        assert refresh() == ["book-a"]
        client_abs.playlists = [{"id": "library-b-adventure", "libraryId": "a", "name": "Adventure"}]
        assert refresh() == ["book-a", "book-b"]
        write_manifest(tmp_path, "book-b")
        # A real upstream failure must preserve both the published view and cache.
        client_abs.failure = True
        with pytest.raises(RuntimeError, match="unavailable"):
            refresh()
        books = client.get("/audiobookshelf/playlist", params={"playlist_id": "garmin-adventure"},
                           headers={"Authorization": "Bearer valid"}).json()
        assert [book["id"] for book in books] == ["book-a", "book-b"]
        assert (tmp_path / "book-b").exists()
        client_abs.failure = False
        client_abs.playlists = []
        assert refresh() == ["book-a"]
        assert not (tmp_path / "book-b").exists()
        client_abs.playlists = [{"id": "library-a-adventure", "libraryId": "a", "name": "Adventure"}]
        assert refresh() == ["book-a"]
    assert not caplog.records


@pytest.mark.asyncio
@pytest.mark.parametrize("status", [404, 401, 403, 500])
async def test_only_playlist_404_is_silently_ignored(monkeypatch, status):
    class Response:
        ok = False

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            pass

        async def text(self):
            return "failure"

    response = Response()
    response.status = status

    class Session(Response):
        def get(self, *args, **kwargs):
            return response

    monkeypatch.setattr("sidecar.abs.aiohttp.ClientSession", Session)
    client = AbsClient("http://abs", "token")
    if status == 404:
        assert await client.playlist_items("deleted") == []
        with pytest.raises(RuntimeError):
            await client._json("/api/libraries")
    else:
        with pytest.raises(RuntimeError, match=str(status)):
            await client.playlist_items("playlist")


@pytest.mark.parametrize("source", ["", "  ", "/Adventure", "Library/  ", 123])
def test_invalid_source_config_rejected(tmp_path, source):
    path = tmp_path / "config.yaml"
    path.write_text(yaml.safe_dump({"virtual_playlists": [
        {"id": "adventure", "name": "Adventure", "source_playlist_ids": [source]},
    ]}))
    with pytest.raises(ValueError, match="invalid source playlist"):
        load_settings(str(path))


def test_config_accepts_mixed_names_and_ids(tmp_path, monkeypatch):
    monkeypatch.setenv("ABS_URL", "http://abs")
    monkeypatch.setenv("ABS_TOKEN", "token")
    path = tmp_path / "config.yaml"
    path.write_text(yaml.safe_dump({"virtual_playlists": [
        {"id": "adventure", "name": "Adventure", "source_playlist_ids": [" Library A/Adventure Books ", "old-id"]},
    ]}))
    assert load_settings(str(path)).playlists[0].source_playlist_ids == ("Library A/Adventure Books", "old-id")
