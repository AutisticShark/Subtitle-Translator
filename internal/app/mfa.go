package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/AutisticShark/Subtitle-Translator/internal/email"
	"github.com/golang-jwt/jwt/v5"
	qrcode "github.com/skip2/go-qrcode"
)

// Password-plus-OTP authentication with SQL-backed, single-use challenges
// (mfa.py). Every operation first locks the user's row and touches only that
// user's rows inside the transaction (rule 29).

const (
	mfaPurgeInterval  = 300
	mfaPurgeBatchSize = 500
	mfaIssuer         = "Subtitle Translator"
)

var (
	errUnreadableTOTP = errors.New("unreadable MFA secret")
	sixDigits         = regexp.MustCompile(`^[0-9]{6}$`)
)

// mfaBudget is a failure budget: (failures column, lock column, challenge
// purposes revoked on lockout). Sign-in and management keep separate budgets so
// a session holder cannot lock the owner out of signing in.
type mfaBudget struct {
	failures, lock string
	purposes       []string
}

var mfaBudgets = map[string]mfaBudget{
	"login":  {"failures", "locked_until", []string{"login"}},
	"manage": {"manage_failures", "manage_locked_until", []string{"enroll", "manage"}},
}

type mfaAccount struct {
	UserID            string
	Method            string
	Secret            string
	Email             string
	LastStep          int64
	RecoveryHashes    string
	Failures          int64
	LockedUntil       int64
	NextSend          int64
	SendWindow        int64
	SendCount         int64
	ManageFailures    int64
	ManageLockedUntil int64
}

func (a *mfaAccount) budgetValues(scope string) (failures, lockedUntil int64) {
	if scope == "manage" {
		return a.ManageFailures, a.ManageLockedUntil
	}
	return a.Failures, a.LockedUntil
}

const mfaAccountColumns = "user_id, method, secret, email, last_step, recovery_hashes, failures, locked_until, " +
	"next_send, send_window, send_count, manage_failures, manage_locked_until"

func loadMFAAccount(ctx context.Context, c conn, userID string) (*mfaAccount, error) {
	var a mfaAccount
	err := c.queryRow(ctx, "SELECT "+mfaAccountColumns+" FROM mfa_accounts WHERE user_id = ?", userID).Scan(
		&a.UserID, &a.Method, &a.Secret, &a.Email, &a.LastStep, &a.RecoveryHashes, &a.Failures, &a.LockedUntil,
		&a.NextSend, &a.SendWindow, &a.SendCount, &a.ManageFailures, &a.ManageLockedUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

type mfaChallenge struct {
	ID, UserID, Purpose, Method, Secret, Email, CodeHash, Transport string
	Version, Expires                                                int64
}

// mfaResponse is decided inside the transaction and written after it commits.
type mfaResponse struct {
	status int
	body   map[string]any
	retry  int64
	// session, when set, signs the user in (cookies unless transport is "header").
	session   *userRow
	transport string
	// deliver marks a challenge response (MFA.deliver): drop the internal
	// challenge id, email deliverCode if set, and purge expired challenges.
	deliver      bool
	deliverCode  string
	deliverEmail string
}

func challengeResponse(body map[string]any, code, email string) *mfaResponse {
	return &mfaResponse{status: http.StatusOK, body: body, deliver: true, deliverCode: code, deliverEmail: email}
}

func (s *Server) mfaNow() int64 { return s.now().Unix() }

func (s *Server) mfaError(locale, message string, status int, retry int64) *mfaResponse {
	if message == "" {
		message = s.tr(locale, "Invalid or expired verification code")
	}
	return &mfaResponse{status: status, body: map[string]any{"error": message}, retry: retry}
}

// lockAccount takes the user row lock with a real UPDATE (which serializes
// writers on SQLite too), then returns the user and their MFA row, creating it.
func (s *Server) lockAccount(ctx context.Context, c conn, userID string, version int64) (*userRow, *mfaAccount, error) {
	if _, err := c.exec(ctx, "UPDATE users SET token_version = token_version WHERE id = ?", userID); err != nil {
		return nil, nil, err
	}
	user, err := loadUser(ctx, c, "id = ?", userID)
	if err != nil || user == nil || !user.Active || user.TokenVersion != version {
		return nil, nil, err
	}
	account, err := loadMFAAccount(ctx, c, userID)
	if err != nil {
		return nil, nil, err
	}
	if account == nil {
		if _, err := c.exec(ctx, "INSERT INTO mfa_accounts ("+mfaAccountColumns+") "+
			"VALUES (?, '', '', '', -1, '[]', 0, 0, 0, 0, 0, 0, 0)", userID); err != nil {
			return nil, nil, err
		}
		if account, err = loadMFAAccount(ctx, c, userID); err != nil {
			return nil, nil, err
		}
	}
	return user, account, nil
}

func (s *Server) guard(locale string, user *userRow, account *mfaAccount, scope string) *mfaResponse {
	if user == nil {
		return s.mfaError(locale, s.tr(locale, "Authentication required"), http.StatusUnauthorized, 0)
	}
	_, lockedUntil := account.budgetValues(scope)
	if remaining := lockedUntil - s.mfaNow(); remaining > 0 {
		return s.mfaError(locale, s.tr(locale, "Too many verification attempts; try again later"),
			http.StatusTooManyRequests, remaining)
	}
	return nil
}

// failed spends one unit of the scope's budget; the update commits even though
// the request fails.
func (s *Server) mfaFailed(ctx context.Context, c conn, locale string, account *mfaAccount, scope, message string,
	status int) (*mfaResponse, error) {
	budget := mfaBudgets[scope]
	failures, _ := account.budgetValues(scope)
	failures++
	var lockedUntil int64
	if failures >= 5 {
		lockedUntil = s.mfaNow() + 900
		failures = 0
	}
	if _, err := c.exec(ctx, "UPDATE mfa_accounts SET "+budget.failures+" = ?, "+budget.lock+" = ? WHERE user_id = ?",
		failures, lockedUntil, account.UserID); err != nil {
		return nil, err
	}
	if lockedUntil != 0 {
		if _, err := c.exec(ctx, "DELETE FROM mfa_challenges WHERE user_id = ? AND purpose IN ("+
			placeholders(len(budget.purposes))+")", append([]any{account.UserID}, stringArgs(budget.purposes)...)...); err != nil {
			return nil, err
		}
	}
	return s.mfaError(locale, message, status, 0), nil
}

// unreadableFactor: an unreadable secret cannot prove a code, but the attempt
// still spends the budget so recovery-code guessing stays rate limited.
func (s *Server) unreadableFactor(ctx context.Context, c conn, locale string, account *mfaAccount,
	scope string) (*mfaResponse, error) {
	return s.mfaFailed(ctx, c, locale, account, scope, s.tr(locale,
		"Your authenticator secret cannot be read. Use a recovery code "+
			"and ask an administrator to restore the encryption key."), http.StatusServiceUnavailable)
}

func (s *Server) resetFailures(ctx context.Context, c conn, userID, scope string) error {
	budget := mfaBudgets[scope]
	_, err := c.exec(ctx, "UPDATE mfa_accounts SET "+budget.failures+" = 0, "+budget.lock+" = 0 WHERE user_id = ?",
		userID)
	return err
}

func mfaPasswordValid(user *userRow, password any) bool {
	text, ok := password.(string)
	if !ok || runeCount(text) > 256 {
		return false
	}
	return verifyPassword(user.PasswordHash, text) == nil
}

func (s *Server) loginResponse(user *userRow, transport string, extra map[string]any) *mfaResponse {
	body := map[string]any{}
	for key, value := range extra {
		body[key] = value
	}
	return &mfaResponse{status: http.StatusOK, body: body, session: user, transport: transport}
}

// writeMFA answers after the transaction committed: issue the session, send
// the email code, and purge other accounts' expired challenges.
func (s *Server) writeMFA(w http.ResponseWriter, r *http.Request, response *mfaResponse) {
	if response.session != nil {
		token, csrf := s.issueToken(response.session, "", 0)
		response.body["user"] = s.publicUser(response.session)
		if response.transport == "header" {
			response.body["access_token"] = token
		} else {
			s.setAccessCookies(w.Header(), token, csrf)
		}
	}
	if response.deliver {
		challengeID, _ := response.body["challenge_id"].(string)
		delete(response.body, "challenge_id")
		if response.deliverCode != "" {
			if s.sendEmailCode(r, response.deliverEmail, response.deliverCode) {
				response.body["email_sent"] = true
			} else {
				// Delivery errors are sanitized; keep provider responses and codes private.
				response.body["email_sent"] = false
				response.body["warning"] = s.tr(localeOf(r), "Email could not be sent. Retry later or use a recovery code.")
				s.forgetUnsentCode(r.Context(), challengeID, response.deliverCode)
			}
		}
		s.purgeExpiredChallenges(r.Context())
	}
	if response.retry > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(max(1, response.retry), 10))
	}
	writeJSON(w, response.status, response.body)
}

func (s *Server) sendEmailCode(r *http.Request, address, code string) bool {
	if s.cfg.Email == nil {
		return false
	}
	locale := localeOf(r)
	subject := s.tr(locale, "Your Subtitle Translator verification code")
	text := s.tr(locale, "Your verification code is {code}. It expires in 5 minutes. If you did not request it, "+
		"ignore this email.", "code", code)
	// The code hash is already committed, so a disconnecting client must not
	// abort the send halfway (the transport bounds it with its own timeout).
	if err := s.cfg.Email.Send(context.WithoutCancel(r.Context()), address, subject, text, ""); err != nil {
		s.logger.Warn("MFA verification email could not be sent")
		return false
	}
	return true
}

// forgetUnsentCode: the client was told the email was not sent, so a later
// sign-in during the cooldown must not claim a code was delivered. The hash
// condition leaves any newer challenge or code untouched.
func (s *Server) forgetUnsentCode(ctx context.Context, challengeID, code string) {
	ctx = context.WithoutCancel(ctx)
	err := s.db.tx(ctx, func(c conn) error {
		_, err := c.exec(ctx, "UPDATE mfa_challenges SET code_hash = '' WHERE id = ? AND code_hash = ?",
			challengeID, s.digestCode(challengeID, code))
		return err
	})
	if err != nil {
		s.logger.Warn("Could not clear an undelivered MFA email code", "error", err)
	}
}

// purgeExpiredChallenges is best-effort removal of every account's expired
// challenges in a separate short transaction, by primary key (no range locks).
func (s *Server) purgeExpiredChallenges(ctx context.Context) {
	now := s.mfaNow()
	s.mfaPurgeMu.Lock()
	if now < s.nextMFAPurge {
		s.mfaPurgeMu.Unlock()
		return
	}
	s.nextMFAPurge = now + mfaPurgeInterval
	s.mfaPurgeMu.Unlock()
	ctx = context.WithoutCancel(ctx)
	err := s.db.tx(ctx, func(c conn) error {
		rows, err := c.query(ctx, "SELECT id FROM mfa_challenges WHERE expires <= ? LIMIT "+
			strconv.Itoa(mfaPurgeBatchSize), now)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		_, err = c.exec(ctx, "DELETE FROM mfa_challenges WHERE id IN ("+placeholders(len(ids))+") AND expires <= ?",
			append(stringArgs(ids), now)...)
		return err
	})
	if err != nil {
		s.logger.Debug("Expired MFA challenge cleanup was skipped", "error", err)
	}
}

// emailLimit allows one code per minute and ten per hour per account.
func (s *Server) emailLimit(ctx context.Context, c conn, locale string, account *mfaAccount) (*mfaResponse, error) {
	now := s.mfaNow()
	message := s.tr(locale, "Please wait before requesting another email code")
	if remaining := account.NextSend - now; remaining > 0 {
		return s.mfaError(locale, message, http.StatusTooManyRequests, remaining), nil
	}
	freshWindow := account.SendWindow+3600 <= now
	if !freshWindow && account.SendCount >= 10 {
		return s.mfaError(locale, message, http.StatusTooManyRequests, account.SendWindow+3600-now), nil
	}
	window, count := account.SendWindow, account.SendCount+1
	if freshWindow {
		window, count = now, 1
	}
	_, err := c.exec(ctx, "UPDATE mfa_accounts SET next_send = ?, send_window = ?, send_count = ? WHERE user_id = ?",
		now+60, window, count, account.UserID)
	return nil, err
}

func (s *Server) digestCode(challengeID, code string) string {
	mac := hmac.New(sha256.New, s.mfaKey)
	mac.Write([]byte(challengeID + "\x00" + code))
	return hex.EncodeToString(mac.Sum(nil))
}

// encodeChallenge signs a challenge JWT with a key derived from the JWT secret,
// so it can never validate as an access token. The signed transport claim lets
// a reissued challenge serve a client that asked for a different transport.
func (s *Server) encodeChallenge(user *userRow, challengeID, purpose, method string, now, expires int64,
	transport string) map[string]any {
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": user.ID, "ver": user.TokenVersion, "cid": challengeID, "purpose": purpose,
		"iat": now, "exp": expires, "tr": transport, "iss": "subtitle-translator", "aud": "subtitle-mfa",
	}).SignedString(s.mfaKey)
	if err != nil {
		panic(err)
	}
	return map[string]any{"challenge_token": token, "method": method, "expires_in": expires - now}
}

func randomDigits() string {
	value, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", value.Int64())
}

// createChallenge replaces the account's purpose challenge and returns the
// response body (with an internal challenge_id) and the code to email. A stored
// email code hash means the code will be sent (see reissueLoginChallenge).
func (s *Server) createChallenge(ctx context.Context, c conn, user *userRow, purpose, method, email, secret,
	transport string, send bool) (map[string]any, string, error) {
	now := s.mfaNow()
	challengeID := randomHex(32)
	code := ""
	if method == "email" && send {
		code = randomDigits()
	}
	expires := now + 300
	if purpose == "enroll" && method == "totp" {
		expires = now + 600
	}
	// Only this account's rows while its user row is locked (rule 29).
	if _, err := c.exec(ctx, "DELETE FROM mfa_challenges WHERE user_id = ? AND (purpose = ? OR expires <= ?)",
		user.ID, purpose, now); err != nil {
		return nil, "", err
	}
	codeHash := ""
	if code != "" {
		codeHash = s.digestCode(challengeID, code)
	}
	if _, err := c.exec(ctx, "INSERT INTO mfa_challenges (id, user_id, version, purpose, method, secret, email, "+
		"code_hash, expires, transport) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		challengeID, user.ID, user.TokenVersion, purpose, method, secret, email, codeHash, expires, transport); err != nil {
		return nil, "", err
	}
	result := s.encodeChallenge(user, challengeID, purpose, method, now, expires, transport)
	result["challenge_id"] = challengeID
	return result, code, nil
}

// reissueLoginChallenge returns a token for the current email login challenge
// without replacing its code. While sending is suppressed, a new challenge would
// delete the code already emailed and never deliver its replacement.
func (s *Server) reissueLoginChallenge(ctx context.Context, c conn, user *userRow,
	transport string) (map[string]any, bool, error) {
	now := s.mfaNow()
	var id, codeHash string
	var expires int64
	err := c.queryRow(ctx, "SELECT id, code_hash, expires FROM mfa_challenges WHERE user_id = ? AND version = ? "+
		"AND purpose = 'login' AND method = 'email' AND expires > ? ORDER BY expires DESC",
		user.ID, user.TokenVersion, now).Scan(&id, &codeHash, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return s.encodeChallenge(user, id, "login", "email", now, expires, transport), codeHash != "", nil
}

func claimedTransport(claims jwt.MapClaims, fallback string) string {
	if value, ok := claims["tr"].(string); ok && (value == "cookies" || value == "header") {
		return value
	}
	return fallback
}

// beginMFALogin finishes a password sign-in: directly without a factor,
// otherwise with a challenge.
func (s *Server) beginMFALogin(w http.ResponseWriter, r *http.Request, original *userRow, payload map[string]any) {
	ctx := r.Context()
	locale := localeOf(r)
	transport := "cookies"
	if payload["token_transport"] == "header" {
		transport = "header"
	}
	var response *mfaResponse
	err := s.db.tx(ctx, func(c conn) error {
		response = nil
		user, account, err := s.lockAccount(ctx, c, original.ID, original.TokenVersion)
		if err != nil {
			return err
		}
		if user == nil || user.PasswordHash != original.PasswordHash {
			response = s.mfaError(locale, s.tr(locale, "Authentication required"), http.StatusUnauthorized, 0)
			return nil
		}
		if account.Method == "" {
			// The MFA lockout protects a second factor; without one there is
			// nothing to guard, and password throttling happens in the caller.
			response = s.loginResponse(user, transport, nil)
			return nil
		}
		if response = s.guard(locale, user, account, "login"); response != nil {
			return nil
		}
		send := account.Method == "email" && s.cfg.Email != nil
		if send {
			limited, err := s.emailLimit(ctx, c, locale, account)
			if err != nil {
				return err
			}
			if limited != nil {
				send = false // Recovery codes remain usable during delivery cooldowns.
			}
		}
		unsent := s.tr(locale, "Email could not be sent. Retry later or use a recovery code.")
		var result map[string]any
		code := ""
		if account.Method == "email" && !send {
			// Keep the code that was already emailed valid instead of replacing it.
			reissued, hasCode, err := s.reissueLoginChallenge(ctx, c, user, transport)
			if err != nil {
				return err
			}
			if reissued != nil {
				result = reissued
				result["email_sent"] = false
				if hasCode {
					result["warning"] = s.tr(locale, "A verification code was requested recently. "+
						"Enter the latest code you received or use a recovery code.")
				} else {
					result["warning"] = unsent
				}
			}
		}
		if result == nil {
			result, code, err = s.createChallenge(ctx, c, user, "login", account.Method, account.Email, "",
				transport, send)
			if err != nil {
				return err
			}
			if account.Method == "email" && !send {
				result["email_sent"] = false
				result["warning"] = unsent
			}
		}
		result["mfa_required"] = true
		response = challengeResponse(result, code, account.Email)
		return nil
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeMFA(w, r, response)
}

// decodeChallenge verifies a challenge JWT; nil means invalid.
func (s *Server) decodeChallenge(payload map[string]any, purpose string) jwt.MapClaims {
	token, ok := payload["challenge_token"].(string)
	if !ok || len(token) > 2048 {
		return nil
	}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience("subtitle-mfa"),
		jwt.WithIssuer("subtitle-translator"), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
		jwt.WithJSONNumber(), jwt.WithTimeFunc(s.now))
	parsed, err := parser.Parse(token, func(*jwt.Token) (any, error) { return s.mfaKey, nil })
	if err != nil {
		return nil
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		return nil
	}
	for _, name := range []string{"sub", "cid", "purpose"} {
		if _, ok := claims[name].(string); !ok {
			return nil
		}
	}
	if _, ok := claimInt(claims, "ver"); !ok {
		return nil
	}
	if _, ok := claims["iat"]; !ok {
		return nil
	}
	if purpose != "" && claims["purpose"] != purpose {
		return nil
	}
	return claims
}

func claimInt(claims jwt.MapClaims, name string) (int64, bool) {
	if number, ok := claims[name].(json.Number); ok {
		value, err := strconv.ParseInt(number.String(), 10, 64)
		return value, err == nil
	}
	return 0, false
}

func claimString(claims jwt.MapClaims, name string) string {
	value, _ := claims[name].(string)
	return value
}

func (s *Server) challengeRow(ctx context.Context, c conn, claims jwt.MapClaims) (*mfaChallenge, error) {
	version, _ := claimInt(claims, "ver")
	var ch mfaChallenge
	err := c.queryRow(ctx, "SELECT id, user_id, version, purpose, method, secret, email, code_hash, expires, transport "+
		"FROM mfa_challenges WHERE id = ? AND user_id = ? AND version = ? AND purpose = ? AND expires > ?",
		claimString(claims, "cid"), claimString(claims, "sub"), version, claimString(claims, "purpose"), s.mfaNow()).Scan(
		&ch.ID, &ch.UserID, &ch.Version, &ch.Purpose, &ch.Method, &ch.Secret, &ch.Email, &ch.CodeHash, &ch.Expires,
		&ch.Transport)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ch, nil
}

// totpAt computes the RFC 6238 code (SHA-1, 6 digits) for a 30-second step.
func totpAt(secret []byte, step int64) string {
	mac := hmac.New(sha1.New, secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", value%1000000)
}

// decodeBase32Secret follows pyotp: pad, case-insensitive.
func decodeBase32Secret(secret string) ([]byte, error) {
	secret = strings.ToUpper(strings.TrimSpace(secret))
	if missing := len(secret) % 8; missing != 0 {
		secret += strings.Repeat("=", 8-missing)
	}
	return base32.StdEncoding.DecodeString(secret)
}

// totpStep returns the matching step after lastStep (current, previous, next).
func (s *Server) totpStep(secret, code string, lastStep int64, userID string) (int64, bool, error) {
	if !sixDigits.MatchString(code) {
		return 0, false, nil
	}
	plain, err := s.decryptSecret(secret)
	var key []byte
	if err == nil {
		key, err = decodeBase32Secret(plain)
	}
	if err != nil {
		// For example JWT_SECRET_KEY was rotated without API_KEY_ENCRYPTION_KEY.
		if _, warned := s.warnOnce.LoadOrStore("mfa-unreadable", true); !warned {
			s.logger.Error("A stored MFA authenticator secret cannot be decrypted; restore the JWT_SECRET_KEY or "+
				"API_KEY_ENCRYPTION_KEY it was saved with. Affected users can sign in only with recovery codes.",
				"user", userID)
		}
		return 0, false, errUnreadableTOTP
	}
	current := s.mfaNow() / 30
	for _, step := range []int64{current, current - 1, current + 1} {
		if step > lastStep && subtle.ConstantTimeCompare([]byte(totpAt(key, step)), []byte(code)) == 1 {
			return step, true, nil
		}
	}
	return 0, false, nil
}

func recoveryHash(code string) string {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(strings.ToLower(code), "-", "")))
	return hex.EncodeToString(sum[:])
}

// checkFactor accepts a single-use recovery code, a TOTP code newer than the
// last accepted step, or the email challenge's code.
func (s *Server) checkFactor(ctx context.Context, c conn, account *mfaAccount, codeValue any,
	challenge *mfaChallenge) (bool, error) {
	code, ok := codeValue.(string)
	if !ok || runeCount(code) > 64 {
		return false, nil
	}
	code = pythonStrip(code)
	var hashes []string
	if err := json.Unmarshal([]byte(account.RecoveryHashes), &hashes); err != nil {
		return false, err
	}
	candidate := recoveryHash(code)
	for i, hash := range hashes {
		if hash == candidate {
			remaining := append(append([]string{}, hashes[:i]...), hashes[i+1:]...)
			_, err := c.exec(ctx, "UPDATE mfa_accounts SET recovery_hashes = ? WHERE user_id = ?",
				pythonJSONStrings(remaining), account.UserID)
			return err == nil, err
		}
	}
	switch {
	case account.Method == "totp":
		step, matched, err := s.totpStep(account.Secret, code, account.LastStep, account.UserID)
		if err != nil || !matched {
			return false, err
		}
		_, err = c.exec(ctx, "UPDATE mfa_accounts SET last_step = ? WHERE user_id = ?", step, account.UserID)
		return err == nil, err
	case account.Method == "email" && challenge != nil && challenge.Method == "email":
		return subtle.ConstantTimeCompare([]byte(s.digestCode(challenge.ID, code)), []byte(challenge.CodeHash)) == 1,
			nil
	}
	return false, nil
}

func maskEmail(address string) string {
	local, domain, found := strings.Cut(address, "@")
	if !found || domain == "" {
		return ""
	}
	runes := []rune(local)
	if len(runes) > 1 {
		runes = runes[:1]
	}
	return string(runes) + "***@" + domain
}

func (s *Server) handleMFAStatus(w http.ResponseWriter, r *http.Request, auth *authResult) {
	account, err := loadMFAAccount(r.Context(), s.db.conn(), auth.claims.sub)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	body := map[string]any{"method": "", "email": "", "recovery_remaining": 0,
		"email_available": s.cfg.Email != nil, "enrollment_available": s.stableKeys}
	if account != nil {
		var hashes []string
		_ = json.Unmarshal([]byte(account.RecoveryHashes), &hashes)
		body["method"] = account.Method
		body["email"] = maskEmail(account.Email)
		body["recovery_remaining"] = len(hashes)
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) tokenVersion(auth *authResult) int64 {
	version, ok := auth.claims.intClaim("ver")
	if !ok {
		return -1
	}
	return version
}

// mfaTx runs a locked MFA operation whose response is decided in the transaction.
func (s *Server) mfaTx(w http.ResponseWriter, r *http.Request, fn func(c conn) (*mfaResponse, error)) {
	var response *mfaResponse
	err := s.db.tx(r.Context(), func(c conn) error {
		var err error
		response, err = fn(c)
		return err
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.writeMFA(w, r, response)
}

func (s *Server) handleMFASetup(w http.ResponseWriter, r *http.Request, auth *authResult) {
	state(r).noRefresh = true
	payload := mfaPayload(r)
	locale := localeOf(r)
	method, _ := payload["method"].(string)
	if method != "totp" && method != "email" {
		s.writeMFA(w, r, s.mfaError(locale, s.tr(locale, "Choose an MFA method"), http.StatusBadRequest, 0))
		return
	}
	if !s.stableKeys {
		s.writeMFA(w, r, s.mfaError(locale, s.tr(locale, "Configure JWT_SECRET_KEY before saving secrets"),
			http.StatusServiceUnavailable, 0))
		return
	}
	email := ""
	if method == "email" {
		if s.cfg.Email == nil {
			s.writeMFA(w, r, s.mfaError(locale, s.tr(locale, "Email verification is unavailable; ask an "+
				"administrator to configure email delivery"), http.StatusServiceUnavailable, 0))
			return
		}
		address, _ := payload["email"].(string)
		if !emailValid(address) {
			s.writeMFA(w, r, s.mfaError(locale, s.tr(locale, "Enter a valid email address"), http.StatusBadRequest, 0))
			return
		}
		email = address
	}
	ctx := r.Context()
	s.mfaTx(w, r, func(c conn) (*mfaResponse, error) {
		user, account, err := s.lockAccount(ctx, c, auth.claims.sub, s.tokenVersion(auth))
		if err != nil {
			return nil, err
		}
		if response := s.guard(locale, user, account, "manage"); response != nil {
			return response, nil
		}
		if !mfaPasswordValid(user, payload["password"]) {
			return s.mfaFailed(ctx, c, locale, account, "manage", s.tr(locale, "Invalid password"), http.StatusBadRequest)
		}
		if account.Method != "" {
			return s.mfaError(locale, s.tr(locale, "Disable your current MFA method before replacing it"),
				http.StatusConflict, 0), nil
		}
		if method == "email" {
			if limited, err := s.emailLimit(ctx, c, locale, account); err != nil || limited != nil {
				return limited, err
			}
		}
		secret, encrypted := "", ""
		if method == "totp" {
			secret = randomBase32Secret()
			encrypted = s.encryptSecret(secret)
		}
		result, code, err := s.createChallenge(ctx, c, user, "enroll", method, email, encrypted, "cookies", true)
		if err != nil {
			return nil, err
		}
		if secret != "" {
			uri := provisioningURI(secret, user.Username)
			result["secret"] = secret
			result["provisioning_uri"] = uri
			svg, err := qrSVG(uri)
			if err != nil {
				return nil, err
			}
			result["qr_code"] = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(svg)
		}
		return challengeResponse(result, code, email), nil
	})
}

func emailValid(address string) bool { return email.ValidAddress(address) }

// randomBase32Secret is pyotp.random_base32(): 32 characters (160 bits).
func randomBase32Secret() string {
	buffer := make([]byte, 20)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return base32.StdEncoding.EncodeToString(buffer)
}

// provisioningURI matches pyotp's TOTP.provisioning_uri(name, issuer_name=...).
func provisioningURI(secret, username string) string {
	quote := func(value string) string { return strings.ReplaceAll(url.QueryEscape(value), "+", "%20") }
	label := quote(mfaIssuer) + ":" + quote(username)
	return "otpauth://totp/" + label + "?secret=" + quote(secret) + "&issuer=" + quote(mfaIssuer)
}

// qrSVG renders a QR code as a compact SVG path with a light background.
func qrSVG(content string) ([]byte, error) {
	code, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	bitmap := code.Bitmap()
	size := len(bitmap)
	var path bytes.Buffer
	for y, row := range bitmap {
		for x := 0; x < len(row); x++ {
			if !row[x] {
				continue
			}
			start := x
			for x < len(row) && row[x] {
				x++
			}
			fmt.Fprintf(&path, "M%d %dh%dv1h-%dz", start, y, x-start, x-start)
		}
	}
	var out bytes.Buffer
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" `+
		`shape-rendering="crispEdges"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="%s"/></svg>`,
		size, size, size*8, size*8, size, size, path.String())
	return out.Bytes(), nil
}

// newRecoveryCodes stores ten fresh hashes and returns the grouped codes.
func newRecoveryCodes(ctx context.Context, c conn, userID string) ([]string, error) {
	codes := make([]string, 10)
	hashes := make([]string, 10)
	for i := range codes {
		raw := randomHex(12)
		sum := sha256.Sum256([]byte(raw))
		hashes[i] = hex.EncodeToString(sum[:])
		codes[i] = raw[0:6] + "-" + raw[6:12] + "-" + raw[12:18] + "-" + raw[18:24]
	}
	_, err := c.exec(ctx, "UPDATE mfa_accounts SET recovery_hashes = ? WHERE user_id = ?",
		pythonJSONStrings(hashes), userID)
	return codes, err
}

// rotateSession ends every other session (token version + 1) and signs the
// current client in again with the same transport it authenticated with.
func (s *Server) rotateSession(ctx context.Context, c conn, user *userRow, auth *authResult,
	extra map[string]any) (*mfaResponse, error) {
	if _, err := c.exec(ctx, "DELETE FROM mfa_challenges WHERE user_id = ?", user.ID); err != nil {
		return nil, err
	}
	if _, err := c.exec(ctx, "UPDATE users SET token_version = token_version + 1, updated_at = ? WHERE id = ?",
		isoSeconds(s.now()), user.ID); err != nil {
		return nil, err
	}
	refreshed, err := loadUser(ctx, c, "id = ?", user.ID)
	if err != nil {
		return nil, err
	}
	transport := "cookies"
	if auth.location == "headers" {
		transport = "header"
	}
	return s.loginResponse(refreshed, transport, extra), nil
}

func (s *Server) handleMFAConfirm(w http.ResponseWriter, r *http.Request, auth *authResult) {
	state(r).noRefresh = true
	payload := mfaPayload(r)
	locale := localeOf(r)
	claims := s.decodeChallenge(payload, "enroll")
	if claims == nil || claimString(claims, "sub") != auth.claims.sub {
		s.writeMFA(w, r, s.mfaError(locale, "", http.StatusBadRequest, 0))
		return
	}
	version, _ := claimInt(claims, "ver")
	if version != s.tokenVersion(auth) {
		s.writeMFA(w, r, s.mfaError(locale, "", http.StatusBadRequest, 0))
		return
	}
	ctx := r.Context()
	s.mfaTx(w, r, func(c conn) (*mfaResponse, error) {
		user, account, err := s.lockAccount(ctx, c, claimString(claims, "sub"), version)
		if err != nil {
			return nil, err
		}
		if response := s.guard(locale, user, account, "manage"); response != nil {
			return response, nil
		}
		challenge, err := s.challengeRow(ctx, c, claims)
		if err != nil {
			return nil, err
		}
		if challenge == nil || account.Method != "" {
			return s.mfaError(locale, "", http.StatusBadRequest, 0), nil
		}
		valid := false
		step := int64(-1)
		if code, ok := payload["code"].(string); ok && runeCount(code) <= 64 {
			code = pythonStrip(code)
			if challenge.Method == "totp" {
				matched, ok, err := s.totpStep(challenge.Secret, code, -1, user.ID)
				if errors.Is(err, errUnreadableTOTP) {
					return s.unreadableFactor(ctx, c, locale, account, "manage")
				}
				if err != nil {
					return nil, err
				}
				valid = ok
				if ok {
					step = matched
				}
			} else {
				valid = subtle.ConstantTimeCompare([]byte(s.digestCode(challenge.ID, code)),
					[]byte(challenge.CodeHash)) == 1
			}
		}
		if !valid {
			return s.mfaFailed(ctx, c, locale, account, "manage", "", http.StatusBadRequest)
		}
		if _, err := c.exec(ctx, "UPDATE mfa_accounts SET method = ?, secret = ?, email = ?, last_step = ? "+
			"WHERE user_id = ?", challenge.Method, challenge.Secret, challenge.Email, step, user.ID); err != nil {
			return nil, err
		}
		if err := s.resetFailures(ctx, c, user.ID, "manage"); err != nil {
			return nil, err
		}
		codes, err := newRecoveryCodes(ctx, c, user.ID)
		if err != nil {
			return nil, err
		}
		return s.rotateSession(ctx, c, user, auth, map[string]any{"recovery_codes": codes})
	})
}

func (s *Server) handleMFAManagementEmail(w http.ResponseWriter, r *http.Request, auth *authResult) {
	state(r).noRefresh = true
	payload := mfaPayload(r)
	locale := localeOf(r)
	ctx := r.Context()
	s.mfaTx(w, r, func(c conn) (*mfaResponse, error) {
		user, account, err := s.lockAccount(ctx, c, auth.claims.sub, s.tokenVersion(auth))
		if err != nil {
			return nil, err
		}
		if response := s.guard(locale, user, account, "manage"); response != nil {
			return response, nil
		}
		if !mfaPasswordValid(user, payload["password"]) {
			return s.mfaFailed(ctx, c, locale, account, "manage", s.tr(locale, "Invalid password"), http.StatusBadRequest)
		}
		if account.Method != "email" || s.cfg.Email == nil {
			return s.mfaError(locale, s.tr(locale, "Email verification is unavailable; ask an administrator to "+
				"configure email delivery"), http.StatusServiceUnavailable, 0), nil
		}
		if limited, err := s.emailLimit(ctx, c, locale, account); err != nil || limited != nil {
			return limited, err
		}
		result, code, err := s.createChallenge(ctx, c, user, "manage", "email", account.Email, "", "cookies", true)
		if err != nil {
			return nil, err
		}
		return challengeResponse(result, code, account.Email), nil
	})
}

// changeMethod disables MFA or replaces recovery codes; both need the password
// and an existing factor, and both end other sessions.
func (s *Server) changeMethod(w http.ResponseWriter, r *http.Request, auth *authResult, regenerate bool) {
	state(r).noRefresh = true
	payload := mfaPayload(r)
	locale := localeOf(r)
	ctx := r.Context()
	s.mfaTx(w, r, func(c conn) (*mfaResponse, error) {
		user, account, err := s.lockAccount(ctx, c, auth.claims.sub, s.tokenVersion(auth))
		if err != nil {
			return nil, err
		}
		if response := s.guard(locale, user, account, "manage"); response != nil {
			return response, nil
		}
		if account.Method == "" {
			return s.mfaError(locale, s.tr(locale, "MFA is not enabled"), http.StatusConflict, 0), nil
		}
		if !mfaPasswordValid(user, payload["password"]) {
			return s.mfaFailed(ctx, c, locale, account, "manage", s.tr(locale, "Invalid password"), http.StatusBadRequest)
		}
		var challenge *mfaChallenge
		if claims := s.decodeChallenge(payload, "manage"); claims != nil {
			version, _ := claimInt(claims, "ver")
			if claimString(claims, "sub") == user.ID && version == user.TokenVersion {
				if challenge, err = s.challengeRow(ctx, c, claims); err != nil {
					return nil, err
				}
			}
		}
		valid, err := s.checkFactor(ctx, c, account, payload["code"], challenge)
		if errors.Is(err, errUnreadableTOTP) {
			return s.unreadableFactor(ctx, c, locale, account, "manage")
		}
		if err != nil {
			return nil, err
		}
		if !valid {
			return s.mfaFailed(ctx, c, locale, account, "manage", "", http.StatusBadRequest)
		}
		if err := s.resetFailures(ctx, c, user.ID, "manage"); err != nil {
			return nil, err
		}
		if regenerate {
			codes, err := newRecoveryCodes(ctx, c, user.ID)
			if err != nil {
				return nil, err
			}
			return s.rotateSession(ctx, c, user, auth, map[string]any{"recovery_codes": codes})
		}
		if _, err := c.exec(ctx, "UPDATE mfa_accounts SET method = '', email = '', secret = '', last_step = -1, "+
			"recovery_hashes = '[]' WHERE user_id = ?", user.ID); err != nil {
			return nil, err
		}
		return s.rotateSession(ctx, c, user, auth, nil)
	})
}

func (s *Server) handleMFADisable(w http.ResponseWriter, r *http.Request, auth *authResult) {
	s.changeMethod(w, r, auth, false)
}

func (s *Server) handleMFARecovery(w http.ResponseWriter, r *http.Request, auth *authResult) {
	s.changeMethod(w, r, auth, true)
}

func (s *Server) handleMFAVerify(w http.ResponseWriter, r *http.Request) {
	state(r).noRefresh = true
	payload := mfaPayload(r)
	locale := localeOf(r)
	claims := s.decodeChallenge(payload, "login")
	if claims == nil {
		s.writeMFA(w, r, s.mfaError(locale, "", http.StatusBadRequest, 0))
		return
	}
	version, _ := claimInt(claims, "ver")
	ctx := r.Context()
	s.mfaTx(w, r, func(c conn) (*mfaResponse, error) {
		user, account, err := s.lockAccount(ctx, c, claimString(claims, "sub"), version)
		if err != nil {
			return nil, err
		}
		if response := s.guard(locale, user, account, "login"); response != nil {
			return response, nil
		}
		challenge, err := s.challengeRow(ctx, c, claims)
		if err != nil {
			return nil, err
		}
		if challenge == nil || account.Method == "" {
			return s.mfaError(locale, "", http.StatusBadRequest, 0), nil
		}
		valid, err := s.checkFactor(ctx, c, account, payload["code"], challenge)
		if errors.Is(err, errUnreadableTOTP) {
			return s.unreadableFactor(ctx, c, locale, account, "login")
		}
		if err != nil {
			return nil, err
		}
		if !valid {
			return s.mfaFailed(ctx, c, locale, account, "login", "", http.StatusBadRequest)
		}
		if _, err := c.exec(ctx, "DELETE FROM mfa_challenges WHERE id = ?", challenge.ID); err != nil {
			return nil, err
		}
		if err := s.resetFailures(ctx, c, user.ID, "login"); err != nil {
			return nil, err
		}
		return s.loginResponse(user, claimedTransport(claims, challenge.Transport), nil), nil
	})
}

func (s *Server) handleMFAResend(w http.ResponseWriter, r *http.Request) {
	state(r).noRefresh = true
	payload := mfaPayload(r)
	locale := localeOf(r)
	claims := s.decodeChallenge(payload, "")
	if claims == nil {
		s.writeMFA(w, r, s.mfaError(locale, "", http.StatusBadRequest, 0))
		return
	}
	version, _ := claimInt(claims, "ver")
	scope := "manage"
	if claimString(claims, "purpose") == "login" {
		scope = "login"
	}
	ctx := r.Context()
	s.mfaTx(w, r, func(c conn) (*mfaResponse, error) {
		user, account, err := s.lockAccount(ctx, c, claimString(claims, "sub"), version)
		if err != nil {
			return nil, err
		}
		if response := s.guard(locale, user, account, scope); response != nil {
			return response, nil
		}
		challenge, err := s.challengeRow(ctx, c, claims)
		if err != nil {
			return nil, err
		}
		if challenge == nil || challenge.Method != "email" {
			return s.mfaError(locale, "", http.StatusBadRequest, 0), nil
		}
		if s.cfg.Email == nil {
			return s.mfaError(locale, s.tr(locale, "Email verification is unavailable; ask an administrator to "+
				"configure email delivery"), http.StatusServiceUnavailable, 0), nil
		}
		if limited, err := s.emailLimit(ctx, c, locale, account); err != nil || limited != nil {
			return limited, err
		}
		// Rotation invalidates both the previous JWT challenge and its email code.
		result, code, err := s.createChallenge(ctx, c, user, challenge.Purpose, "email", challenge.Email, "",
			claimedTransport(claims, challenge.Transport), true)
		if err != nil {
			return nil, err
		}
		return challengeResponse(result, code, challenge.Email), nil
	})
}
