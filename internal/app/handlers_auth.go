package app

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

var accountThemes = map[string]bool{"system": true, "light": true, "dark": true}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	count, err := s.db.conn().scalarInt(ctx, "SELECT COUNT(*) FROM users")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	settings, err := s.readSettings(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	body := settings.publicAuthConfiguration()
	body["configured"] = count > 0
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	state(r).noRefresh = true
	payload, ok := s.authPayload(w, r)
	if !ok {
		return
	}
	locale := localeOf(r)
	username := usernameFrom(payload["username"])
	password := payload["password"]
	if message := s.validateUsername(locale, username); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	if message := s.validatePassword(locale, password); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	ctx := r.Context()
	userID := uuidHex()
	alreadyComplete := false
	err := s.db.tx(ctx, func(c conn) error {
		alreadyComplete = false
		count, err := c.scalarInt(ctx, "SELECT COUNT(*) FROM users")
		if err != nil {
			return err
		}
		if count > 0 {
			alreadyComplete = true
			return errRollback
		}
		return s.createFirstAdmin(ctx, c, userID, username, password.(string), isoSeconds(s.now()))
	})
	if alreadyComplete || (err != nil && isIntegrityError(err)) {
		s.jsonError(w, r, http.StatusConflict, "Initial setup is already complete")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.respondNewSession(w, r, userID)
}

// respondNewSession answers setup and registration: the user, signed in with cookies.
func (s *Server) respondNewSession(w http.ResponseWriter, r *http.Request, userID string) {
	user, err := loadUser(r.Context(), s.db.conn(), "id = ?", userID)
	if err != nil || user == nil {
		s.internalError(w, r, err)
		return
	}
	token, csrf := s.issueToken(user, "", 0)
	s.setAccessCookies(w.Header(), token, csrf)
	writeJSON(w, http.StatusCreated, map[string]any{"user": s.publicUser(user)})
}

// reserveAccountLoginAttempt reserves one password guess before the slow hash
// with a single conditional UPDATE; it counts as a failure until a success
// resets it, so parallel requests cannot exceed the lockout budget.
func (s *Server) reserveAccountLoginAttempt(ctx context.Context, user *userRow) (bool, error) {
	var reserved bool
	err := s.db.tx(ctx, func(c conn) error {
		condition, args := observedLockCondition(user.LockedUntil.String, user.LockedUntil.Valid)
		changed, err := c.exec(ctx, "UPDATE users SET failed_login_count = failed_login_count + 1, updated_at = ? "+
			"WHERE id = ? AND active = ? AND failed_login_count < ? AND "+condition,
			append([]any{isoSeconds(s.now()), user.ID, true, loginFailureLimit}, args...)...)
		reserved = changed == 1
		return err
	})
	return reserved, err
}

// observedLockCondition matches the lock state read before verification, or a
// lock already cleared; a lock written concurrently has a different value.
func observedLockCondition(lockedUntil string, valid bool) (string, []any) {
	if !valid {
		return "locked_until IS NULL", nil
	}
	return "(locked_until IS NULL OR locked_until = ?)", []any{lockedUntil}
}

// lockExhaustedAccount locks an account whose reserved attempts reached the limit.
func (s *Server) lockExhaustedAccount(ctx context.Context, userID string) error {
	return s.db.tx(ctx, func(c conn) error {
		now := s.now()
		_, err := c.exec(ctx, "UPDATE users SET locked_until = ?, failed_login_count = 0, updated_at = ? "+
			"WHERE id = ? AND failed_login_count >= ?",
			isoSeconds(now.Add(loginLockMinutes*time.Minute)), isoSeconds(now), userID, loginFailureLimit)
		return err
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	state(r).noRefresh = true
	payload, ok := s.authPayload(w, r)
	if !ok {
		return
	}
	// The attempt reservation, the lock and the reset are separate transactions
	// around the slow hash; a client that disconnects in between must not stop
	// them. A committed reservation without its lock would leave the counter at
	// the limit and the account unusable until an administrator unlocks it.
	ctx := context.WithoutCancel(r.Context())
	remote := state(r).remote
	if remote == "" {
		remote = "unknown"
	}
	if s.loginLimit.limited(remote) {
		s.jsonError(w, r, http.StatusTooManyRequests, "Too many login attempts; try again later")
		return
	}
	failure, err := s.verifyCaptcha(ctx, r, "login", payload["captcha_token"])
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"error": failure.message})
		return
	}
	// Reserve the per-address attempt before the slow password check.
	marker, ok := s.loginLimit.reserve(remote)
	if !ok {
		s.jsonError(w, r, http.StatusTooManyRequests, "Too many login attempts; try again later")
		return
	}
	rawUsername, usernameOK := payload["username"].(string)
	password, passwordOK := payload["password"].(string)
	validInput := usernameOK && passwordOK && runeCount(rawUsername) <= 64 && runeCount(password) <= 256
	username, candidate := "", ""
	if validInput {
		username, candidate = normalizeUsername(rawUsername), password
	}
	user, err := loadUser(ctx, s.db.conn(), "username = ?", username)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	locked := false
	if user != nil && user.LockedUntil.Valid {
		if until, ok := parseTimestamp(user.LockedUntil.String); ok && until.After(s.now()) {
			locked = true
		}
	}
	reserved := false
	if user != nil && validInput && user.Active && !locked {
		if reserved, err = s.reserveAccountLoginAttempt(ctx, user); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	// Without a reservation the outcome is already a failure; hash the dummy
	// value so the response time does not reveal whether the account is locked.
	hash := s.dummyHash
	if reserved {
		hash = user.PasswordHash
	}
	verified := verifyPassword(hash, candidate) == nil && reserved
	invalid := func() { s.jsonError(w, r, http.StatusUnauthorized, "Invalid username or password") }
	if !verified {
		if reserved {
			if err := s.lockExhaustedAccount(ctx, user.ID); err != nil {
				s.internalError(w, r, err)
				return
			}
		}
		invalid()
		return
	}
	var refreshed *userRow
	err = s.db.tx(ctx, func(c conn) error {
		refreshed = nil
		set := "failed_login_count = 0, locked_until = NULL, updated_at = ?"
		args := []any{isoSeconds(s.now())}
		if passwordNeedsRehash(user.PasswordHash) {
			set += ", password_hash = ?"
			args = append(args, hashPassword(candidate))
		}
		// Conditional on the lock state observed before verification, so a lock
		// set by concurrent failures is never cleared by this success.
		condition, conditionArgs := observedLockCondition(user.LockedUntil.String, user.LockedUntil.Valid)
		args = append(args, user.ID, true, user.TokenVersion, user.PasswordHash)
		changed, err := c.exec(ctx, "UPDATE users SET "+set+" WHERE id = ? AND active = ? AND token_version = ? "+
			"AND password_hash = ? AND "+condition, append(args, conditionArgs...)...)
		if err != nil {
			return err
		}
		if changed == 0 {
			return errRollback
		}
		refreshed, err = loadUser(ctx, c, "id = ?", user.ID)
		return err
	})
	if errors.Is(err, errRollback) || (err == nil && refreshed == nil) {
		invalid()
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.loginLimit.release(remote, marker)
	s.beginMFALogin(w, r.WithContext(ctx), refreshed, payload)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	state(r).noRefresh = true
	payload, ok := s.authPayload(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	remote := state(r).remote
	if remote == "" {
		remote = "unknown"
	}
	if _, ok := s.registerLimit.reserve(remote); !ok {
		s.jsonError(w, r, http.StatusTooManyRequests, "Too many registration attempts; try again later")
		return
	}
	settings, err := s.readSettings(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if settings.get("registration_enabled") != "1" {
		s.jsonError(w, r, http.StatusForbidden, "New account registration is disabled")
		return
	}
	count, err := s.db.conn().scalarInt(ctx, "SELECT COUNT(*) FROM users")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if count == 0 {
		s.jsonError(w, r, http.StatusConflict, "Create the first administrator before registering users")
		return
	}
	failure, err := s.verifyCaptcha(ctx, r, "register", payload["captcha_token"])
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if failure != nil {
		writeJSON(w, failure.status, map[string]any{"error": failure.message})
		return
	}
	locale := localeOf(r)
	username := usernameFrom(payload["username"])
	password := payload["password"]
	if message := s.validateUsername(locale, username); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	if message := s.validatePassword(locale, password); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	if confirm, ok := payload["confirm_password"].(string); !ok || confirm != password.(string) {
		s.jsonError(w, r, http.StatusBadRequest, "Passwords do not match")
		return
	}
	userID := uuidHex()
	err = s.db.tx(ctx, func(c conn) error {
		return insertUser(ctx, c, userID, username, hashPassword(password.(string)), "user", isoSeconds(s.now()))
	})
	if err != nil && isIntegrityError(err) {
		s.jsonError(w, r, http.StatusConflict, "Username already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.respondNewSession(w, r, userID)
}

// revokeTokenKey records a revoked jti or session key; repeats are harmless.
func (s *Server) revokeTokenKey(ctx context.Context, key string, expires time.Time) error {
	err := s.db.tx(ctx, func(c conn) error {
		count, err := c.scalarInt(ctx, "SELECT COUNT(*) FROM revoked_tokens WHERE jti = ?", key)
		if err != nil || count > 0 {
			return err
		}
		_, err = c.exec(ctx, "INSERT INTO revoked_tokens (jti, expires_at, created_at) VALUES (?, ?, ?)",
			key, isoSeconds(expires), isoSeconds(s.now()))
		return err
	})
	if err != nil && isIntegrityError(err) {
		return nil // A concurrent logout recorded the same key first.
	}
	return err
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, auth *authResult) {
	state(r).noRefresh = true
	// Both keys must be recorded even if the client goes away between them.
	ctx := context.WithoutCancel(r.Context())
	expires := time.Unix(auth.claims.exp, 0)
	if err := s.revokeTokenKey(ctx, auth.claims.jti, expires); err != nil {
		s.internalError(w, r, err)
		return
	}
	if sid := auth.claims.sessionID(); sid != "" {
		// Revoke the whole session: earlier refreshed tokens, and any token an
		// in-flight request mints for it, which expires no later than this bound.
		bound := expires
		if later := s.now().Add(s.accessTTL); later.After(bound) {
			bound = later
		}
		if err := s.revokeTokenKey(ctx, revokedSessionPrefix+sid, bound.Add(time.Minute)); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.unsetAccessCookies(w.Header())
	writeJSON(w, http.StatusOK, map[string]any{"message": s.tr(localeOf(r), "Logged out")})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, auth *authResult) {
	user, err := loadUser(r.Context(), s.db.conn(), "id = ?", auth.user.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if user == nil {
		s.jsonError(w, r, http.StatusUnauthorized, "User not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": s.publicUser(user)})
}

func unknownFields(payload map[string]any, allowed ...string) string {
	var unknown []string
	for key := range payload {
		if !contains(allowed, key) {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return strings.Join(unknown, ", ")
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request, auth *authResult) {
	payload, ok := s.jsonPayload(w, r)
	if !ok {
		return
	}
	if unknown := unknownFields(payload, "theme"); unknown != "" {
		s.jsonError(w, r, http.StatusBadRequest, "Unknown fields: {fields}", "fields", unknown)
		return
	}
	theme, ok := payload["theme"].(string)
	if !ok || !accountThemes[theme] {
		s.jsonError(w, r, http.StatusBadRequest, "Theme must be system, light, or dark")
		return
	}
	ctx := r.Context()
	err := s.db.tx(ctx, func(c conn) error {
		_, err := c.exec(ctx, "UPDATE users SET theme = ?, updated_at = ? WHERE id = ?",
			theme, isoSeconds(s.now()), auth.user.ID)
		return err
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.handleMe(w, r, auth)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request, _ *authResult) {
	ctx := r.Context()
	rows, err := s.db.conn().query(ctx, "SELECT "+prefixed("users", userColumns)+", "+
		"(SELECT COUNT(jobs.id) FROM jobs WHERE jobs.user_id = users.id) AS job_count FROM users ORDER BY users.username")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer rows.Close()
	users := []map[string]any{}
	for rows.Next() {
		var count int64
		user, err := scanUser(rows, &count)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		user.JobCount = &count
		users = append(users, s.publicUser(user))
	}
	if err := rows.Err(); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users})
}

func prefixed(table, columns string) string {
	parts := strings.Split(columns, ", ")
	for i, part := range parts {
		parts[i] = table + "." + part
	}
	return strings.Join(parts, ", ")
}

func validRole(value any) bool {
	role, ok := value.(string)
	return ok && (role == "user" || role == "admin")
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request, _ *authResult) {
	payload, ok := s.jsonPayload(w, r)
	if !ok {
		return
	}
	locale := localeOf(r)
	username := usernameFrom(payload["username"])
	password := payload["password"]
	role, present := payload["role"]
	if !present {
		role = "user"
	}
	if message := s.validateUsername(locale, username); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	if message := s.validatePassword(locale, password); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	if !validRole(role) {
		s.jsonError(w, r, http.StatusBadRequest, "Invalid role")
		return
	}
	ctx := r.Context()
	userID := uuidHex()
	err := s.db.tx(ctx, func(c conn) error {
		return insertUser(ctx, c, userID, username, hashPassword(password.(string)), role.(string), isoSeconds(s.now()))
	})
	if err != nil && isIntegrityError(err) {
		s.jsonError(w, r, http.StatusConflict, "Username already exists")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	user, err := loadUser(ctx, s.db.conn(), "id = ?", userID)
	if err != nil || user == nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": s.publicUser(user)})
}

// lockAdminRoster serializes changes that can remove an active administrator:
// writing the shared row takes a row lock on PostgreSQL/MariaDB/MySQL and the
// write lock on SQLite, so concurrent changes re-count administrators one at a time.
func lockAdminRoster(ctx context.Context, c conn) error {
	_, err := c.exec(ctx, "UPDATE settings SET updated_at = updated_at WHERE name = ?", adminRosterLockSetting)
	return err
}

// actorStillAdmin re-checks, inside the roster lock, that the actor was not revoked.
func actorStillAdmin(ctx context.Context, c conn, actor *userRow) (bool, error) {
	current, err := loadUser(ctx, c, "id = ?", actor.ID)
	if err != nil || current == nil {
		return false, err
	}
	return current.Role == "admin" && current.Active && current.TokenVersion == actor.TokenVersion, nil
}

func activeAdminCount(ctx context.Context, c conn) (int64, error) {
	return c.scalarInt(ctx, "SELECT COUNT(*) FROM users WHERE role = 'admin' AND active = ?", true)
}

// txResponse lets a transaction body decide the HTTP response and roll back.
type txResponse struct {
	status  int
	message string
}

func (t *txResponse) Error() string { return t.message }

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request, auth *authResult) {
	targetID := r.PathValue("user_id")
	payload, ok := s.jsonPayload(w, r)
	if !ok {
		return
	}
	if unknown := unknownFields(payload, "role", "active", "password", "unlock"); unknown != "" {
		s.jsonError(w, r, http.StatusBadRequest, "Unknown fields: {fields}", "fields", unknown)
		return
	}
	locale := localeOf(r)
	set := []string{"updated_at = ?"}
	args := []any{isoSeconds(s.now())}
	role, hasRole := payload["role"]
	if hasRole {
		if !validRole(role) {
			s.jsonError(w, r, http.StatusBadRequest, "Invalid role")
			return
		}
		set, args = append(set, "role = ?"), append(args, role)
	}
	active, hasActive := payload["active"]
	if hasActive {
		if _, ok := active.(bool); !ok {
			s.jsonError(w, r, http.StatusBadRequest, "active must be a boolean")
			return
		}
		set, args = append(set, "active = ?"), append(args, active)
	}
	bumpVersion := false
	if password, ok := payload["password"]; ok {
		if message := s.validatePassword(locale, password); message != "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
			return
		}
		set, args = append(set, "password_hash = ?"), append(args, hashPassword(password.(string)))
		bumpVersion = true
	}
	unlockValue, hasUnlock := payload["unlock"]
	if hasUnlock {
		if _, ok := unlockValue.(bool); !ok {
			s.jsonError(w, r, http.StatusBadRequest, "unlock must be a boolean")
			return
		}
	}
	unlock := unlockValue == true
	if unlock {
		set = append(set, "failed_login_count = 0", "locked_until = NULL")
	}
	deactivates := hasActive && active == false
	demotes := hasRole && role == "user"
	if targetID == auth.user.ID && (deactivates || demotes) {
		s.jsonError(w, r, http.StatusConflict, "You cannot deactivate or demote your own account")
		return
	}
	if hasActive || hasRole {
		bumpVersion = true
	}
	if bumpVersion {
		set = append(set, "token_version = token_version + 1")
	}
	ctx := r.Context()
	err := s.db.tx(ctx, func(c conn) error {
		if err := lockAdminRoster(ctx, c); err != nil {
			return err
		}
		still, err := actorStillAdmin(ctx, c, auth.user)
		if err != nil {
			return err
		}
		if !still {
			return &txResponse{http.StatusForbidden, "Administrator access required"}
		}
		target, err := loadUser(ctx, c, "id = ?", targetID)
		if err != nil {
			return err
		}
		if target == nil {
			return &txResponse{http.StatusNotFound, "User not found"}
		}
		if target.Role == "admin" && target.Active && (deactivates || demotes) {
			count, err := activeAdminCount(ctx, c)
			if err != nil {
				return err
			}
			if count <= 1 {
				return &txResponse{http.StatusConflict, "At least one active administrator is required"}
			}
		}
		if _, err := c.exec(ctx, "UPDATE users SET "+strings.Join(set, ", ")+" WHERE id = ?",
			append(args, targetID)...); err != nil {
			return err
		}
		if unlock {
			// MFA verification locks are separate from the password lock; an
			// administrator unlock clears all of them (factors stay enrolled).
			_, err := c.exec(ctx, "UPDATE mfa_accounts SET failures = 0, locked_until = 0, manage_failures = 0, "+
				"manage_locked_until = 0 WHERE user_id = ?", targetID)
			return err
		}
		return nil
	})
	if s.writeTxError(w, r, err) {
		return
	}
	updated, err := loadUser(ctx, s.db.conn(), "id = ?", targetID)
	if err != nil || updated == nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": s.publicUser(updated)})
}

// writeTxError answers a failed transaction; it reports whether it responded.
func (s *Server) writeTxError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var response *txResponse
	if errors.As(err, &response) {
		s.jsonError(w, r, response.status, response.message)
		return true
	}
	s.internalError(w, r, err)
	return true
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request, auth *authResult) {
	targetID := r.PathValue("user_id")
	if targetID == auth.user.ID {
		s.jsonError(w, r, http.StatusConflict, "You cannot delete your own account")
		return
	}
	ctx := r.Context()
	err := s.db.tx(ctx, func(c conn) error {
		if err := lockAdminRoster(ctx, c); err != nil {
			return err
		}
		still, err := actorStillAdmin(ctx, c, auth.user)
		if err != nil {
			return err
		}
		if !still {
			return &txResponse{http.StatusForbidden, "Administrator access required"}
		}
		target, err := loadUser(ctx, c, "id = ?", targetID)
		if err != nil {
			return err
		}
		if target == nil {
			return &txResponse{http.StatusNotFound, "User not found"}
		}
		if target.Role == "admin" && target.Active {
			count, err := activeAdminCount(ctx, c)
			if err != nil {
				return err
			}
			if count <= 1 {
				return &txResponse{http.StatusConflict, "At least one active administrator is required"}
			}
		}
		active, err := c.scalarInt(ctx, "SELECT COUNT(*) FROM jobs WHERE user_id = ? AND status IN "+
			"('queued', 'processing', 'canceling')", targetID)
		if err != nil {
			return err
		}
		if active > 0 {
			return &txResponse{http.StatusConflict, "Cancel or finish this user's active jobs first"}
		}
		if _, err := c.exec(ctx, "UPDATE jobs SET user_id = NULL WHERE user_id = ?", targetID); err != nil {
			return err
		}
		scopes := []string{"user:" + targetID}
		for _, period := range calendarPeriods {
			scopes = append(scopes, "user:"+targetID+":"+period)
		}
		if _, err := c.exec(ctx, "DELETE FROM rate_limit_buckets WHERE scope IN ("+placeholders(len(scopes))+")",
			stringArgs(scopes)...); err != nil {
			return err
		}
		// Explicit, because legacy SQLite databases may lack ON DELETE CASCADE.
		for _, table := range []string{"mfa_challenges", "mfa_accounts"} {
			if _, err := c.exec(ctx, "DELETE FROM "+table+" WHERE user_id = ?", targetID); err != nil {
				return err
			}
		}
		if _, err := c.exec(ctx, "DELETE FROM users WHERE id = ?", targetID); err != nil {
			return err
		}
		return bumpCacheRevision(ctx, c, "jobs")
	})
	if s.writeTxError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": targetID})
}
