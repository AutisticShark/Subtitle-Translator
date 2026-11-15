package app

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"time"
)

// calendarQuotaWindow returns the UTC day, ISO week (from Monday) or month.
func calendarQuotaWindow(period string, timestamp time.Time) (time.Time, time.Time) {
	t := timestamp.UTC()
	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "daily":
		return start, start.AddDate(0, 0, 1)
	case "weekly":
		weekday := (int(start.Weekday()) + 6) % 7 // Monday is 0
		start = start.AddDate(0, 0, -weekday)
		return start, start.AddDate(0, 0, 7)
	default:
		start = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0)
	}
}

type quotaBucket struct {
	scope   string
	started string
	used    int64
}

type quotaCheck struct {
	scope   string // "user", "admin" or "panel"
	limit   int64
	started time.Time
	used    int64
	reset   time.Time
	period  string // "window", "daily", "weekly" or "monthly"
	bucket  string
}

// consumeJobQuota atomically consumes amount job units or describes the
// exceeded limit (quotas.consume_job_quota). It must run in the transaction
// that inserts the jobs; it writes the shared panel_job_limit row first, which
// serializes submissions across processes and database backends.
func (s *Server) consumeJobQuota(ctx context.Context, c conn, locale string, user *userRow,
	amount int) (map[string]any, error) {
	if err := lockSettings(ctx, c); err != nil {
		return nil, err
	}
	// Read the time after acquiring the lock, including when waiting across a reset.
	now := s.now()
	timestamp := isoSeconds(now)
	keys := rateLimitKeys()
	rows, err := c.query(ctx, "SELECT name, value FROM settings WHERE name IN ("+placeholders(len(keys))+")",
		stringArgs(keys)...)
	if err != nil {
		return nil, err
	}
	stored := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			rows.Close()
			return nil, err
		}
		stored[name] = value
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	limits := map[string]int64{}
	for _, key := range keys {
		raw, ok := stored[key]
		if !ok {
			raw = s.defaults[key]
		}
		value, err := strconv.ParseInt(pythonStrip(raw), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("stored setting %s is not a whole number", key)
		}
		limits[key] = value
	}
	window := time.Duration(limits["rate_limit_window_minutes"]) * time.Minute
	accountScope := "user:" + user.ID
	scopes := []string{"panel", accountScope}
	for _, period := range calendarPeriods {
		scopes = append(scopes, "panel:"+period, accountScope+":"+period)
	}
	bucketRows, err := c.query(ctx, "SELECT scope, window_started_at, used FROM rate_limit_buckets WHERE scope IN ("+
		placeholders(len(scopes))+")", stringArgs(scopes)...)
	if err != nil {
		return nil, err
	}
	buckets := map[string]quotaBucket{}
	for bucketRows.Next() {
		var bucket quotaBucket
		var used sql.NullInt64
		if err := bucketRows.Scan(&bucket.scope, &bucket.started, &used); err != nil {
			bucketRows.Close()
			return nil, err
		}
		bucket.used = used.Int64
		buckets[bucket.scope] = bucket
	}
	bucketRows.Close()
	if err := bucketRows.Err(); err != nil {
		return nil, err
	}
	windowBucket := func(scope string) (time.Time, int64) {
		row, ok := buckets[scope]
		if !ok {
			return now, 0
		}
		started, valid := parseTimestamp(row.started)
		if !valid || !now.Before(started.Add(window)) {
			return now, 0
		}
		return started, row.used
	}
	panelStarted, panelUsed := windowBucket("panel")
	accountStarted, accountUsed := windowBucket(accountScope)
	accountKey := "user_job_limit"
	if user.Role == "admin" {
		accountKey = "admin_job_limit"
	}
	checks := []quotaCheck{
		{user.Role, limits[accountKey], accountStarted, accountUsed, accountStarted.Add(window), "window", accountScope},
		{"panel", limits["panel_job_limit"], panelStarted, panelUsed, panelStarted.Add(window), "window", "panel"},
	}
	for _, period := range calendarPeriods {
		started, reset := calendarQuotaWindow(period, now)
		for _, pair := range [][2]string{{user.Role, accountScope}, {"panel", "panel"}} {
			key := pair[1] + ":" + period
			var used int64
			if row, ok := buckets[key]; ok {
				if parsed, valid := parseTimestamp(row.started); valid && parsed.Equal(started) {
					used = row.used
				}
			}
			checks = append(checks, quotaCheck{pair[0], limits[pair[0]+"_"+period+"_job_limit"], started, used,
				reset, period, key})
		}
	}
	var exceeded map[string]any
	bestRetry := int64(-1)
	for _, check := range checks {
		if check.limit == 0 || check.used+int64(amount) <= check.limit {
			continue
		}
		retryAfter := max(int64(1), int64(math.Ceil(check.reset.Sub(now).Seconds())))
		label := s.tr(locale, "Panel-wide")
		switch check.scope {
		case "admin":
			label = s.tr(locale, "Administrator")
		case "user":
			label = s.tr(locale, "Regular-user")
		}
		var message string
		if check.period == "window" {
			minutes := limits["rate_limit_window_minutes"]
			message = s.tr(locale, "{label} rate limit of {limit} translation job{job_plural} per "+
				"{minutes} minute{minute_plural} exceeded",
				"label", label, "limit", check.limit, "job_plural", plural(check.limit),
				"minutes", minutes, "minute_plural", plural(minutes))
		} else {
			periodLabel := map[string]string{
				"daily": s.tr(locale, "Daily"), "weekly": s.tr(locale, "Weekly"), "monthly": s.tr(locale, "Monthly"),
			}[check.period]
			message = s.tr(locale, "{label} {period} translation limit of {limit} jobs exceeded. Resets at {reset} (UTC).",
				"label", label, "period", periodLabel, "limit", check.limit,
				"reset", check.reset.UTC().Format("2006-01-02 15:04"))
		}
		if retryAfter > bestRetry {
			bestRetry = retryAfter
			exceeded = map[string]any{
				"error": message, "scope": check.scope, "limit": check.limit, "retry_after": retryAfter,
				"period": check.period, "used": check.used, "requested": amount, "reset_at": isoSeconds(check.reset),
			}
		}
	}
	if exceeded != nil {
		return exceeded, nil
	}
	for _, check := range checks {
		started := isoSeconds(check.started)
		if _, ok := buckets[check.bucket]; ok {
			_, err = c.exec(ctx, "UPDATE rate_limit_buckets SET window_started_at = ?, used = ?, updated_at = ? "+
				"WHERE scope = ?", started, check.used+int64(amount), timestamp, check.bucket)
		} else {
			_, err = c.exec(ctx, "INSERT INTO rate_limit_buckets (scope, window_started_at, used, updated_at) "+
				"VALUES (?, ?, ?, ?)", check.bucket, started, check.used+int64(amount), timestamp)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func plural(count int64) string {
	if count != 1 {
		return "s"
	}
	return ""
}
