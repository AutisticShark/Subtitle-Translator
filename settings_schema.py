"""Declarative definitions and validation for administrator-editable panel settings.

This module has no Flask, SQL, or request dependencies: it validates plain values so
the rules can be unit-tested and kept in one place instead of scattered through the
route handler. Stored values remain strings; validation guarantees that every value
the worker later converts with ``int()`` or ``float()`` is convertible.
"""

from __future__ import annotations

import re
from typing import Any, Iterable

from i18n import translate as tr


CALENDAR_PERIODS = ("daily", "weekly", "monthly")
CALENDAR_LIMIT_KEYS = {
    f"{scope}_{period}_job_limit"
    for scope in ("user", "admin", "panel") for period in CALENDAR_PERIODS
}
WINDOW_LIMIT_KEYS = {
    "rate_limit_window_minutes", "user_job_limit", "admin_job_limit",
    "panel_job_limit",
}
RATE_LIMIT_KEYS = WINDOW_LIMIT_KEYS | CALENDAR_LIMIT_KEYS

CAPTCHA_PROVIDERS = ("turnstile", "recaptcha", "hcaptcha")
CAPTCHA_ACTION_SETTINGS = {
    "login": "captcha_on_login",
    "register": "captcha_on_register",
    "upload": "captcha_on_upload",
}
BOOLEAN_SETTINGS = {"registration_enabled", *CAPTCHA_ACTION_SETTINGS.values()}

# Inclusive (minimum, maximum) bounds. Integer settings must be written as whole
# numbers ("4", not "4.0" or "4.5") because the job worker reads them with int().
NUMBER_SETTINGS = {"rpm": (0, 10000), "width": (4, 80)}
INTEGER_SETTINGS = {
    "batch_size": (1, 100),
    "workers": (1, 16),
    "max_lines": (1, 5),
    "rate_limit_window_minutes": (1, 10080),
    "user_job_limit": (0, 100000),
    "admin_job_limit": (0, 100000),
    "panel_job_limit": (0, 1000000),
    **{key: (0, 1000000 if key.startswith("panel_") else 100000)
       for key in CALENDAR_LIMIT_KEYS},
}

_HOSTNAME_PATTERN = re.compile(
    r"(?=.{1,253}\Z)(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*"
    r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
)


def is_scalar_setting(value: Any) -> bool:
    """Return whether ``value`` is a JSON string or number (never a boolean or null)."""
    return isinstance(value, (str, int, float)) and not isinstance(value, bool)


def validate_setting_types(payload: dict[str, Any]) -> str | None:
    """Reject values that ``str()`` would silently store as text such as "True" or "None".

    Every setting is persisted as the stripped ``str()`` of its JSON value, so only
    strings and numbers have a meaningful stored form. Booleans, nulls, arrays, and
    objects are rejected before any other rule inspects (or hashes) the value.
    """
    for key in sorted(payload):
        if not is_scalar_setting(payload[key]):
            return tr("{key} must be text or a number", key=key)
    return None


def validate_choice_and_flag_settings(
    payload: dict[str, Any], providers: Iterable[str],
) -> str | None:
    """Validate provider choices, on/off flags, and the CAPTCHA hostname.

    A valid ``captcha_hostname`` is normalized in place (trimmed, lowercased, without
    a trailing dot). Returns a translated error message, or ``None`` when valid.
    """
    error = validate_setting_types(payload)
    if error:
        return error
    if payload.get("default_provider") and payload["default_provider"] not in tuple(providers):
        return tr("Invalid default provider")
    if ("captcha_provider" in payload
            and str(payload["captcha_provider"]).strip() not in {"none", *CAPTCHA_PROVIDERS}):
        return tr("Invalid CAPTCHA provider")
    for key in BOOLEAN_SETTINGS:
        if key in payload and str(payload[key]).strip() not in {"0", "1"}:
            return tr("{key} must be enabled or disabled", key=key)
    if "captcha_hostname" in payload:
        hostname = str(payload["captcha_hostname"]).strip().lower().rstrip(".")
        if hostname and not _HOSTNAME_PATTERN.fullmatch(hostname):
            return tr("CAPTCHA hostname must be a hostname without a scheme or port")
        payload["captcha_hostname"] = hostname
    return None


def validate_numeric_settings(payload: dict[str, Any]) -> str | None:
    """Validate numeric and integer bounds. Returns an error message or ``None``.

    Each rule checks the exact stripped text that will be stored, so the worker's
    later ``int()``/``float()`` conversion of that text is guaranteed to succeed.
    """
    error = validate_setting_types(
        {key: value for key, value in payload.items()
         if key in NUMBER_SETTINGS or key in INTEGER_SETTINGS}
    )
    if error:
        return error
    try:
        for key, (minimum, maximum) in NUMBER_SETTINGS.items():
            if key in payload and not minimum <= float(str(payload[key]).strip()) <= maximum:
                return tr(
                    "{key} must be between {minimum} and {maximum}",
                    key=key, minimum=minimum, maximum=maximum,
                )
    except (TypeError, ValueError) as exc:
        return str(exc)
    for key, (minimum, maximum) in INTEGER_SETTINGS.items():
        if key not in payload:
            continue
        text = str(payload[key]).strip()
        try:
            value = int(text)
        except (TypeError, ValueError):
            value = None
        if value is None or str(value) != text or not minimum <= value <= maximum:
            return tr(
                "{key} must be a whole number between {minimum} and {maximum}",
                key=key, minimum=minimum, maximum=maximum,
            )
    return None
