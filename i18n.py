"""Small, dependency-free localization helpers for the web application."""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Iterable

from flask import has_request_context, request


DEFAULT_LOCALE = "en"
LOCALE_COOKIE = "ui_locale"
LOCALES_DIR = Path(__file__).resolve().parent / "locales"


def _load_catalogs() -> tuple[dict[str, dict[str, str]], dict[str, str], dict[str, str]]:
    catalogs: dict[str, dict[str, str]] = {}
    labels: dict[str, str] = {}
    aliases: dict[str, str] = {}
    for path in sorted(LOCALES_DIR.glob("*.json")):
        payload = json.loads(path.read_text("utf-8"))
        locale = str(payload.get("locale") or path.stem)
        messages = payload.get("messages", {})
        if not isinstance(messages, dict) or not all(
            isinstance(key, str) and isinstance(value, str)
            for key, value in messages.items()
        ):
            raise RuntimeError(f"Invalid localization catalog: {path}")
        catalogs[locale] = messages
        labels[locale] = str(payload.get("name") or locale)
        aliases[locale.lower().replace("_", "-")] = locale
        for alias in payload.get("aliases", []):
            aliases[str(alias).lower().replace("_", "-")] = locale
    if DEFAULT_LOCALE not in catalogs:
        raise RuntimeError(f"Missing default localization catalog: {DEFAULT_LOCALE}")
    return catalogs, labels, aliases


CATALOGS, LOCALE_LABELS, LOCALE_ALIASES = _load_catalogs()


def normalize_locale(value: Any) -> str | None:
    """Return a supported canonical locale for a user-supplied locale tag."""
    candidate = str(value or "").strip().lower().replace("_", "-")
    if not candidate:
        return None
    direct = LOCALE_ALIASES.get(candidate)
    if direct:
        return direct
    primary = candidate.split("-", 1)[0]
    return LOCALE_ALIASES.get(primary)


def select_locale(
    query_locale: Any = None,
    cookie_locale: Any = None,
    accepted_locales: Iterable[Any] = (),
) -> str:
    """Resolve locale priority: explicit query, persisted cookie, then browser header."""
    for candidate in (query_locale, cookie_locale, *accepted_locales):
        locale = normalize_locale(candidate)
        if locale:
            return locale
    return DEFAULT_LOCALE


def current_locale() -> str:
    if not has_request_context():
        return DEFAULT_LOCALE
    accepted = (value for value, quality in request.accept_languages if quality > 0)
    return select_locale(
        request.args.get("lang"), request.cookies.get(LOCALE_COOKIE), accepted,
    )


def messages_for(locale: str | None = None) -> dict[str, str]:
    return dict(CATALOGS.get(locale or current_locale(), {}))


def translate(message: str, **values: Any) -> str:
    translated = CATALOGS.get(current_locale(), {}).get(message, message)
    return translated.format(**values) if values else translated
