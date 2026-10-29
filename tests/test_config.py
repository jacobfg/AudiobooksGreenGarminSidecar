import pytest

from sidecar.config import load_settings


@pytest.fixture
def config_path(tmp_path, monkeypatch):
    monkeypatch.setenv("ABS_URL", "http://abs/")
    monkeypatch.delenv("ABS_TOKEN", raising=False)
    monkeypatch.delenv("ABS_TOKEN_FILE", raising=False)
    path = tmp_path / "config.yaml"
    path.write_text(
        "virtual_playlists:\n"
        "  - id: adventure\n"
        "    name: Adventure\n"
        "    source_playlist_ids: [playlist-id]\n",
        encoding="utf-8",
    )
    return str(path)


def test_token_environment(config_path, monkeypatch):
    monkeypatch.setenv("ABS_TOKEN", "env-token")
    assert load_settings(config_path).abs_token == "env-token"


def test_token_file(config_path, tmp_path, monkeypatch):
    token_file = tmp_path / "secret"
    token_file.write_text(" file-token\n", encoding="utf-8")
    monkeypatch.setenv("ABS_TOKEN_FILE", str(token_file))
    assert load_settings(config_path).abs_token == "file-token"


def test_conflicting_tokens(config_path, monkeypatch):
    monkeypatch.setenv("ABS_TOKEN", "env-token")
    monkeypatch.setenv("ABS_TOKEN_FILE", "/run/secrets/abs_token")
    with pytest.raises(ValueError, match="Set only one"):
        load_settings(config_path)


@pytest.mark.parametrize("contents", ["", " \n\t"])
def test_empty_token_file(config_path, tmp_path, monkeypatch, contents):
    token_file = tmp_path / "secret"
    token_file.write_text(contents, encoding="utf-8")
    monkeypatch.setenv("ABS_TOKEN_FILE", str(token_file))
    with pytest.raises(ValueError, match="non-empty ABS_TOKEN_FILE"):
        load_settings(config_path)


@pytest.mark.parametrize("kind", ["missing", "directory", "invalid-utf8"])
def test_unreadable_token_file(config_path, tmp_path, monkeypatch, kind):
    token_file = tmp_path / "secret"
    if kind == "directory":
        token_file.mkdir()
    elif kind == "invalid-utf8":
        token_file.write_bytes(b"\xff")
    monkeypatch.setenv("ABS_TOKEN_FILE", str(token_file))
    with pytest.raises(ValueError, match="readable UTF-8 token file"):
        load_settings(config_path)


def test_missing_token(config_path):
    with pytest.raises(ValueError, match="ABS_URL and either ABS_TOKEN"):
        load_settings(config_path)
