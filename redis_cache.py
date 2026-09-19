"""Optional, disposable Redis cache. SQL remains the source of truth."""

from __future__ import annotations

import hashlib
import json
import logging
import os
import threading
import time
from typing import Any, Callable

from cryptography.fernet import Fernet, InvalidToken
from redis import Redis
from redis.backoff import NoBackoff
from redis.exceptions import RedisError
from redis.retry import Retry


LOGGER = logging.getLogger(__name__)


class RedisCache:
    def __init__(self, client: Redis | None, cipher: Fernet, prefix: str,
                 ttl: int = 60):
        self.client = client
        self.cipher = cipher
        self.prefix = prefix
        self.ttl = ttl
        self._retry_at = 0.0
        self._lock = threading.Lock()

    @classmethod
    def from_environment(cls, cipher: Fernet, database_identity: str) -> RedisCache:
        url = os.environ.get("REDIS_URL", "").strip()
        ttl = int(os.environ.get("REDIS_CACHE_TTL", "60"))
        if not 1 <= ttl <= 3600:
            raise ValueError("REDIS_CACHE_TTL must be between 1 and 3600 seconds")
        namespace = os.environ.get("REDIS_KEY_PREFIX", "subtitle-translator").strip()
        identity = hashlib.sha256(database_identity.encode()).hexdigest()[:24]
        client = None
        if url:
            try:
                client = Redis.from_url(
                    url, socket_connect_timeout=0.25, socket_timeout=0.25,
                    retry=Retry(NoBackoff(), 0), max_connections=16,
                )
            except (ValueError, TypeError):
                # Never include the URL, which can contain credentials.
                raise ValueError("REDIS_URL must be a valid Redis connection URL") from None
        return cls(client, cipher, f"{namespace}:v1:{identity}", ttl)

    @property
    def available(self) -> bool:
        return self.client is not None and time.monotonic() >= self._retry_at

    def _failed(self) -> None:
        with self._lock:
            if time.monotonic() >= self._retry_at:
                LOGGER.warning("Redis cache unavailable; using the database for 5 seconds")
            self._retry_at = time.monotonic() + 5

    def remember(self, key: str, loader: Callable[[], Any]) -> Any:
        if not self.available:
            return loader()
        cache_key = f"{self.prefix}:{key}"
        try:
            raw = self.client.get(cache_key)
        except (RedisError, OSError):
            self._failed()
            return loader()
        if raw is not None:
            try:
                # Encrypt metadata and bind the authenticated payload to its key.
                # Moving a valid value to another user's key must not expose it.
                envelope = json.loads(self.cipher.decrypt(raw, ttl=self.ttl))
                if isinstance(envelope, dict) and envelope.get("key") == cache_key:
                    return envelope["value"]
            except (InvalidToken, ValueError, TypeError, KeyError):
                pass
        value = loader()
        encoded = self.cipher.encrypt(json.dumps({
            "key": cache_key, "value": value,
        }, ensure_ascii=False).encode())
        try:
            self.client.set(cache_key, encoded, ex=self.ttl)
        except (RedisError, OSError):
            self._failed()
        return value
