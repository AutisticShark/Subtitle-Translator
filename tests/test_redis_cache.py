from unittest.mock import Mock

import pytest
from cryptography.fernet import Fernet
from redis.exceptions import ConnectionError

from redis_cache import RedisCache


def fake_client():
    client = Mock()
    client.values = {}
    client.get.side_effect = client.values.get

    def put(key, value, *, ex):
        assert ex > 0
        client.values[key] = value
        return True

    client.set.side_effect = put
    return client


def test_hits_expiry_and_encrypted_key_binding(monkeypatch):
    client = fake_client()
    cipher = Fernet(Fernet.generate_key())
    cache = RedisCache(client, cipher, "test", ttl=60)
    loader = Mock(return_value={"secret": "private"})
    assert cache.remember("alice", loader) == {"secret": "private"}
    assert cache.remember("alice", loader) == {"secret": "private"}
    loader.assert_called_once()
    assert b"private" not in client.values["test:alice"]
    assert client.set.call_args.kwargs == {"ex": 60}

    # Even a valid encrypted value copied to another account's key is a miss.
    client.values["test:bob"] = client.values["test:alice"]
    assert cache.remember("bob", lambda: {"secret": "bob"}) == {"secret": "bob"}
    client.values["test:alice"] = cipher.encrypt_at_time(
        b'{"key":"test:alice","value":"expired"}', current_time=1,
    )
    assert cache.remember("alice", lambda: "fresh") == "fresh"
    client.values["test:alice"] = b"corrupted"
    assert cache.remember("alice", lambda: "repaired") == "repaired"


def test_read_write_outages_cooldown_and_recovery(monkeypatch, caplog):
    client = fake_client()
    cache = RedisCache(client, Fernet(Fernet.generate_key()), "test")
    client.get.side_effect = ConnectionError("credential-must-not-be-logged")
    monkeypatch.setattr("redis_cache.time.monotonic", lambda: 10)
    assert cache.remember("key", lambda: 1) == 1
    assert cache.remember("key", lambda: 2) == 2
    assert client.get.call_count == 1
    assert "credential-must-not-be-logged" not in caplog.text
    assert "using the database" in caplog.text

    monkeypatch.setattr("redis_cache.time.monotonic", lambda: 16)
    client.get.side_effect = client.values.get
    client.set.side_effect = ConnectionError("write failed")
    assert cache.remember("key", lambda: 3) == 3
    assert not cache.available


def test_disabled_cache_never_connects_and_configuration_is_bounded(monkeypatch):
    monkeypatch.setenv("REDIS_URL", "")
    monkeypatch.setenv("REDIS_CACHE_TTL", "60")
    cipher = Fernet(Fernet.generate_key())
    cache = RedisCache.from_environment(cipher, "sqlite:///isolated.db")
    assert not cache.available
    assert cache.remember("key", lambda: "sql") == "sql"
    for invalid in ("0", "3601", "nonsense"):
        monkeypatch.setenv("REDIS_CACHE_TTL", invalid)
        with pytest.raises(ValueError):
            RedisCache.from_environment(cipher, "db")


def test_database_namespaces_and_connection_options(monkeypatch):
    monkeypatch.setenv("REDIS_URL", "rediss://user:password@localhost:6379/1")
    monkeypatch.setenv("REDIS_CACHE_TTL", "60")
    cipher = Fernet(Fernet.generate_key())
    one = RedisCache.from_environment(cipher, "db-one")
    two = RedisCache.from_environment(cipher, "db-two")
    assert one.prefix != two.prefix
    options = one.client.connection_pool.connection_kwargs
    assert options["socket_timeout"] == 0.25
    assert options["socket_connect_timeout"] == 0.25
    assert one.client.connection_pool.max_connections == 16
