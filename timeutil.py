"""Small timestamp helpers shared by the web application modules."""

from __future__ import annotations

from datetime import datetime, timezone


def parse_timestamp(value: str | None) -> datetime | None:
    if not value:
        return None
    try:
        parsed = datetime.fromisoformat(value)
    except ValueError:
        return None
    return parsed.replace(tzinfo=parsed.tzinfo or timezone.utc)
