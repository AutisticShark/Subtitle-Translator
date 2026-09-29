"""Translation-job quota accounting: rolling-window and UTC calendar limits.

``consume_job_quota`` must run inside the same database transaction as the job
inserts. It takes the shared ``panel_job_limit`` settings row first, which
serializes submissions across workers and database backends.
"""

from __future__ import annotations

import math
from datetime import datetime, timedelta, timezone
from typing import Any, Callable

from sqlalchemy import insert, select, update

from database import rate_limit_buckets
from database import settings as settings_table
from i18n import translate as tr
from settings_schema import CALENDAR_PERIODS, RATE_LIMIT_KEYS
from timeutil import parse_timestamp


def calendar_quota_window(period: str, timestamp: datetime) -> tuple[datetime, datetime]:
    start = timestamp.astimezone(timezone.utc).replace(hour=0, minute=0, second=0, microsecond=0)
    if period == "daily":
        return start, start + timedelta(days=1)
    if period == "weekly":
        start -= timedelta(days=start.weekday())
        return start, start + timedelta(days=7)
    if period == "monthly":
        start = start.replace(day=1)
        return start, (start + timedelta(days=32)).replace(day=1)
    raise ValueError("Unknown quota period")


def consume_job_quota(
    db, user: Any, amount: int, *, defaults: dict[str, str], clock: Callable[[], datetime],
) -> dict[str, Any] | None:
    """Atomically consume job quota or describe the exceeded limit."""
    # Every job submission and rate-setting update takes this database row lock first.
    # It serializes the panel counter across application workers and database backends.
    db.execute(update(settings_table).where(
        settings_table.c.name == "panel_job_limit"
    ).values(updated_at=settings_table.c.updated_at))
    # Read time after acquiring the lock, including when waiting across a reset.
    timestamp_datetime = clock()
    timestamp = timestamp_datetime.isoformat(timespec="seconds")
    stored = dict(db.execute(select(
        settings_table.c.name, settings_table.c.value
    ).where(settings_table.c.name.in_(RATE_LIMIT_KEYS))).all())
    limits = {key: int(stored.get(key, defaults[key])) for key in RATE_LIMIT_KEYS}
    window = timedelta(minutes=limits["rate_limit_window_minutes"])
    account_scope = f"user:{user.id}"
    scopes = ["panel", account_scope] + [
        f"{scope}:{period}" for period in CALENDAR_PERIODS
        for scope in ("panel", account_scope)
    ]
    rows = {
        row.scope: row
        for row in db.execute(select(rate_limit_buckets).where(
            rate_limit_buckets.c.scope.in_(scopes)
        )).all()
    }

    def bucket(scope: str) -> tuple[datetime, int]:
        row = rows.get(scope)
        started = parse_timestamp(row.window_started_at) if row else None
        if started is None or timestamp_datetime >= started + window:
            return timestamp_datetime, 0
        return started, int(row.used)

    panel_started, panel_used = bucket("panel")
    account_started, account_used = bucket(account_scope)
    account_key = "admin_job_limit" if user.role == "admin" else "user_job_limit"
    checks = [
        (user.role, limits[account_key], account_started, account_used,
         account_started + window, "window", account_scope),
        ("panel", limits["panel_job_limit"], panel_started, panel_used,
         panel_started + window, "window", "panel"),
    ]
    for period in CALENDAR_PERIODS:
        started, reset = calendar_quota_window(period, timestamp_datetime)
        for scope, bucket_scope in ((user.role, account_scope), ("panel", "panel")):
            bucket_key = f"{bucket_scope}:{period}"
            row = rows.get(bucket_key)
            used = int(row.used) if row and parse_timestamp(row.window_started_at) == started else 0
            checks.append((scope, limits[f"{scope}_{period}_job_limit"],
                           started, used, reset, period, bucket_key))
    exceeded = []
    for scope, limit, _started, used, reset, period, _bucket_key in checks:
        if limit and used + amount > limit:
            retry_after = max(1, math.ceil((reset - timestamp_datetime).total_seconds()))
            label = tr("Administrator") if scope == "admin" else (
                tr("Regular-user") if scope == "user" else tr("Panel-wide")
            )
            if period == "window":
                error = tr(
                    "{label} rate limit of {limit} translation job{job_plural} per "
                    "{minutes} minute{minute_plural} exceeded",
                    label=label, limit=limit, job_plural="s" if limit != 1 else "",
                    minutes=limits["rate_limit_window_minutes"],
                    minute_plural=(
                        "s" if limits["rate_limit_window_minutes"] != 1 else ""
                    ),
                )
            else:
                period_label = {"daily": tr("Daily"), "weekly": tr("Weekly"),
                                "monthly": tr("Monthly")}[period]
                error = tr(
                    "{label} {period} translation limit of {limit} jobs exceeded. Resets at {reset} (UTC).",
                    label=label, period=period_label, limit=limit,
                    reset=reset.strftime("%Y-%m-%d %H:%M"),
                )
            exceeded.append({
                "error": error,
                "scope": scope,
                "limit": limit,
                "retry_after": retry_after,
                "period": period,
                "used": used,
                "requested": amount,
                "reset_at": reset.isoformat(timespec="seconds"),
            })
    if exceeded:
        return max(exceeded, key=lambda item: item["retry_after"])

    for _role, _limit, started, used, _reset, _period, scope in checks:
        values = {
            "window_started_at": started.isoformat(timespec="seconds"),
            "used": used + amount,
            "updated_at": timestamp,
        }
        if scope in rows:
            db.execute(update(rate_limit_buckets).where(
                rate_limit_buckets.c.scope == scope
            ).values(**values))
        else:
            db.execute(insert(rate_limit_buckets).values(scope=scope, **values))
    return None
