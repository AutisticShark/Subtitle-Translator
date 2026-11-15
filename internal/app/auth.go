package app

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Cookie and header names of Flask-JWT-Extended's defaults, kept so browsers
// and API clients of the Python version keep working.
const (
	accessCookieName = "access_token_cookie"
	csrfCookieName   = "csrf_access_token"
	csrfHeaderName   = "X-CSRF-TOKEN"
	// cookieMaxAge is Flask-JWT-Extended's persistent-cookie lifetime (one year);
	// the token inside expires much sooner.
	cookieMaxAge = 31540000
)

func randomURLToken(n int) string {
	buffer := make([]byte, n)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

// userRow is a users table row.
type userRow struct {
	ID               string
	Username         string
	PasswordHash     string
	Role             string
	Theme            string
	Active           bool
	TokenVersion     int64
	FailedLoginCount int64
	LockedUntil      sql.NullString
	CreatedAt        string
	UpdatedAt        string
	JobCount         *int64
}

const userColumns = "id, username, password_hash, role, theme, active, token_version, " +
	"failed_login_count, locked_until, created_at, updated_at"

func scanUser(scanner interface{ Scan(...any) error }, extra ...any) (*userRow, error) {
	var user userRow
	targets := []any{&user.ID, &user.Username, &user.PasswordHash, &user.Role, &user.Theme, &user.Active,
		&user.TokenVersion, &user.FailedLoginCount, &user.LockedUntil, &user.CreatedAt, &user.UpdatedAt}
	if err := scanner.Scan(append(targets, extra...)...); err != nil {
		return nil, err
	}
	return &user, nil
}

// loadUser returns nil without error when the user does not exist.
func loadUser(ctx context.Context, c conn, where string, args ...any) (*userRow, error) {
	user, err := scanUser(c.queryRow(ctx, "SELECT "+userColumns+" FROM users WHERE "+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return user, err
}

func insertUser(ctx context.Context, c conn, id, username, passwordHash, role, timestamp string) error {
	_, err := c.exec(ctx, "INSERT INTO users (id, username, password_hash, role, theme, active, token_version, "+
		"failed_login_count, locked_until, created_at, updated_at) VALUES (?, ?, ?, ?, 'system', ?, 0, 0, NULL, ?, ?)",
		id, username, passwordHash, role, true, timestamp, timestamp)
	return err
}

func (s *Server) publicUser(user *userRow) map[string]any {
	locked := false
	if user.LockedUntil.Valid {
		if until, ok := parseTimestamp(user.LockedUntil.String); ok && until.After(s.now()) {
			locked = true
		}
	}
	theme := user.Theme
	if theme == "" {
		theme = "system"
	}
	result := map[string]any{
		"id": user.ID, "username": user.Username, "role": user.Role, "theme": theme,
		"active": user.Active, "locked": locked, "created_at": user.CreatedAt,
	}
	if user.JobCount != nil {
		result["job_count"] = *user.JobCount
	}
	return result
}

// --- validation ---------------------------------------------------------------

// usernamePattern: 3-64 characters that start and end with a letter or digit.
// Only account creation validates it; existing accounts keep their names.
var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{1,62}[a-z0-9]$`)

func normalizeUsername(value string) string { return strings.ToLower(pythonStrip(value)) }

// pythonStrip is str.strip(): Unicode whitespace including the \x1c-\x1f separators.
func pythonStrip(value string) string {
	return strings.TrimFunc(value, isPythonSpace)
}

func isPythonSpace(r rune) bool {
	return (r >= 0x1c && r <= 0x1f) || isUnicodeSpace(r)
}

func usernameFrom(value any) string {
	if text, ok := value.(string); ok {
		return normalizeUsername(text)
	}
	if value == nil {
		return ""
	}
	text, _ := pythonStr(value)
	return normalizeUsername(text)
}

func (s *Server) validateUsername(locale, username string) string {
	if !usernamePattern.MatchString(username) {
		return s.tr(locale, "Username must be 3-64 lowercase letters, numbers, dots, dashes, or underscores")
	}
	return ""
}

func (s *Server) validatePassword(locale string, password any) string {
	text, ok := password.(string)
	if !ok || runeCount(text) < passwordMinLength {
		return s.tr(locale, "Password must contain at least {minimum} characters", "minimum", passwordMinLength)
	}
	if runeCount(text) > 256 {
		return s.tr(locale, "Password is too long")
	}
	return ""
}

func runeCount(value string) int { return len([]rune(value)) }

// --- access tokens ------------------------------------------------------------

// accessClaims are the verified claims of an access JWT.
type accessClaims struct {
	raw map[string]any
	sub string
	jti string
	exp int64
}

func (c accessClaims) intClaim(name string) (int64, bool) {
	switch value := c.raw[name].(type) {
	case json.Number:
		if parsed, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func (c accessClaims) stringClaim(name string) string {
	if value, ok := c.raw[name].(string); ok {
		return value
	}
	return ""
}

// sessionStarted is the Unix time the session began: sst, or iat for tokens
// issued before sst existed (so the upgrade bounds them instead of logging out).
func (c accessClaims) sessionStarted() (int64, bool) {
	if value, ok := c.intClaim("sst"); ok {
		return value, true
	}
	return c.intClaim("iat")
}

// sessionID is sid, or the jti of tokens issued before sid existed.
func (c accessClaims) sessionID() string {
	if sid := c.stringClaim("sid"); sid != "" {
		return sid
	}
	return c.stringClaim("jti")
}

func (c accessClaims) revocationKeys() []string {
	keys := []string{c.jti}
	if sid := c.sessionID(); sid != "" {
		keys = append(keys, revokedSessionPrefix+sid)
	}
	return keys
}

// issueToken creates an access JWT like Flask-JWT-Extended's
// create_access_token(fresh=True). A sign-in starts a new session; refreshes
// keep sessionID and started, and no token outlives started + SESSION_MAX_HOURS.
func (s *Server) issueToken(user *userRow, sessionID string, started int64) (token, csrf string) {
	now := s.now()
	if started == 0 {
		started = now.Unix()
	}
	if sessionID == "" {
		sessionID = randomHex(16)
	}
	remaining := time.Unix(started, 0).Add(s.sessionMax).Sub(now)
	lifetime := max(time.Second, min(s.accessTTL, remaining))
	csrf = uuid4()
	claims := jwt.MapClaims{
		"fresh": true,
		"iat":   now.Unix(),
		"jti":   uuid4(),
		"type":  "access",
		"sub":   user.ID,
		"nbf":   now.Unix(),
		"csrf":  csrf,
		"exp":   now.Add(lifetime).Unix(),
		"role":  user.Role,
		"ver":   user.TokenVersion,
		"sid":   sessionID,
		"sst":   started,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtSecret)
	if err != nil {
		panic(err)
	}
	return signed, csrf
}

func (s *Server) setAccessCookies(h http.Header, token, csrf string) {
	expires := s.now().Add(cookieMaxAge * time.Second)
	for _, cookie := range []*http.Cookie{
		{Name: accessCookieName, Value: token, HttpOnly: true},
		{Name: csrfCookieName, Value: csrf},
	} {
		cookie.Path = "/"
		cookie.MaxAge = cookieMaxAge
		cookie.Expires = expires
		cookie.Secure = s.cfg.CookieSecure
		cookie.SameSite = http.SameSiteStrictMode
		h.Add("Set-Cookie", cookie.String())
	}
}

func (s *Server) unsetAccessCookies(h http.Header) {
	for _, cookie := range []*http.Cookie{
		{Name: accessCookieName, HttpOnly: true},
		{Name: csrfCookieName},
	} {
		cookie.Path = "/"
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(0, 0)
		cookie.Secure = s.cfg.CookieSecure
		cookie.SameSite = http.SameSiteStrictMode
		h.Add("Set-Cookie", cookie.String())
	}
}

func responseSetsAccessCookie(h http.Header) bool {
	for _, value := range h.Values("Set-Cookie") {
		if strings.HasPrefix(value, accessCookieName+"=") {
			return true
		}
	}
	return false
}

// authFailure is a 401 reason with Flask-JWT-Extended's handler mapping.
type authFailure struct {
	kind   string // missing, invalid, expired, revoked
	detail string
}

func (f *authFailure) Error() string { return f.kind + ": " + f.detail }

var commaSplit = regexp.MustCompile(`,\s*`)

var csrfMethods = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// authResult is a successfully authenticated request.
type authResult struct {
	claims   accessClaims
	user     *userRow
	location string // "cookies" or "headers"
}

// authenticate follows Flask-JWT-Extended with JWT_TOKEN_LOCATION
// ["cookies", "headers"]: the cookie wins when present (and then needs the CSRF
// header on unsafe methods), otherwise an "Authorization: Bearer" header. The
// token must also still be current (currentTokenUser).
func (s *Server) authenticate(r *http.Request) (*authResult, *authFailure) {
	var missing []string
	if cookie, err := r.Cookie(accessCookieName); err == nil && cookie.Value != "" {
		csrf := ""
		if csrfMethods[r.Method] {
			csrf = r.Header.Get(csrfHeaderName)
			if csrf == "" {
				return nil, &authFailure{"missing", "Missing CSRF token"}
			}
		}
		claims, failure := s.decodeAccessToken(cookie.Value, csrf, csrfMethods[r.Method])
		if failure != nil {
			return nil, failure
		}
		return s.finishAuthentication(r.Context(), claims, "cookies")
	}
	missing = append(missing, fmt.Sprintf("Missing cookie %q", accessCookieName))
	header := strings.Trim(strings.TrimSpace(r.Header.Get("Authorization")), ",")
	if header == "" {
		missing = append(missing, "Missing Authorization Header")
		return nil, &authFailure{"missing", "Missing JWT in cookies or headers (" + strings.Join(missing, "; ") + ")"}
	}
	var bearer []string
	for _, field := range regexp.MustCompile(`,\s*`).Split(header, -1) {
		if parts := strings.Fields(field); len(parts) > 0 && parts[0] == "Bearer" {
			bearer = append(bearer, field)
		}
	}
	if len(bearer) != 1 {
		missing = append(missing, "Missing 'Bearer' type in 'Authorization' header. Expected 'Authorization: Bearer <JWT>'")
		return nil, &authFailure{"missing", "Missing JWT in cookies or headers (" + strings.Join(missing, "; ") + ")"}
	}
	parts := strings.Fields(bearer[0])
	if len(parts) != 2 {
		return nil, &authFailure{"invalid", "Bad Authorization header. Expected 'Authorization: Bearer <JWT>'"}
	}
	claims, failure := s.decodeAccessToken(parts[1], "", false)
	if failure != nil {
		return nil, failure
	}
	return s.finishAuthentication(r.Context(), claims, "headers")
}

func (s *Server) finishAuthentication(ctx context.Context, claims accessClaims, location string) (*authResult, *authFailure) {
	user, err := s.currentTokenUser(ctx, s.db.conn(), claims)
	if err != nil {
		return nil, &authFailure{"error", err.Error()}
	}
	if user == nil {
		return nil, &authFailure{"revoked", "Token has been revoked"}
	}
	return &authResult{claims: claims, user: user, location: location}, nil
}

func (s *Server) decodeAccessToken(token, csrf string, checkCSRF bool) (accessClaims, *authFailure) {
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithJSONNumber(),
		jwt.WithIssuedAt(), jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.now))
	parsed, err := parser.Parse(token, func(*jwt.Token) (any, error) { return s.jwtSecret, nil })
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return accessClaims{}, &authFailure{"expired", "Token has expired"}
		}
		return accessClaims{}, &authFailure{"invalid", tokenErrorDetail(err)}
	}
	raw, _ := parsed.Claims.(jwt.MapClaims)
	claims := accessClaims{raw: raw}
	sub, ok := raw["sub"].(string)
	if !ok {
		return accessClaims{}, &authFailure{"invalid", "Missing claim: sub"}
	}
	claims.sub = sub
	claims.jti = claims.stringClaim("jti")
	if exp, ok := raw["exp"].(json.Number); ok {
		value, err := exp.Float64()
		if err != nil {
			return accessClaims{}, &authFailure{"invalid", "Invalid exp claim"}
		}
		claims.exp = int64(math.Floor(value))
	}
	if kind, ok := raw["type"].(string); ok && kind == "refresh" {
		return accessClaims{}, &authFailure{"invalid", "Only non-refresh tokens are allowed"}
	}
	if checkCSRF {
		tokenCSRF, ok := raw["csrf"].(string)
		if !ok {
			return accessClaims{}, &authFailure{"invalid", "Missing claim: csrf"}
		}
		if subtle.ConstantTimeCompare([]byte(tokenCSRF), []byte(csrf)) != 1 {
			return accessClaims{}, &authFailure{"missing", "CSRF double submit tokens do not match"}
		}
	}
	return claims, nil
}

func tokenErrorDetail(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "Not enough segments"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid):
		return "Signature verification failed"
	case errors.Is(err, jwt.ErrTokenNotValidYet), errors.Is(err, jwt.ErrTokenUsedBeforeIssued):
		return "The token is not yet valid"
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return "The specified alg value is not allowed"
	}
	return "Invalid token"
}

// currentTokenUser is the one definition of token validity shared by request
// authentication and cookie refresh (rule 22): the user is active, ver equals
// the current token version, and neither the jti nor the session id is revoked.
func (s *Server) currentTokenUser(ctx context.Context, c conn, claims accessClaims) (*userRow, error) {
	keys := claims.revocationKeys()
	count, err := c.scalarInt(ctx, "SELECT COUNT(*) FROM revoked_tokens WHERE jti IN ("+placeholders(len(keys))+")",
		stringArgs(keys)...)
	if err != nil || count > 0 {
		return nil, err
	}
	user, err := loadUser(ctx, c, "id = ?", claims.sub)
	if err != nil || user == nil || !user.Active {
		return nil, err
	}
	version, ok := claims.intClaim("ver")
	if !ok {
		if text := claims.stringClaim("ver"); text != "" {
			parsed, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
			if err != nil {
				return nil, nil
			}
			version, ok = parsed, true
		}
	}
	if !ok || version != user.TokenVersion {
		return nil, nil
	}
	return user, nil
}

func (s *Server) authFailureResponse(w http.ResponseWriter, r *http.Request, failure *authFailure) {
	locale := localeOf(r)
	switch failure.kind {
	case "missing":
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": s.tr(locale, "Authentication required"), "detail": failure.detail})
	case "invalid":
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": s.tr(locale, "Invalid authentication token"), "detail": failure.detail})
	case "expired":
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": s.tr(locale, "Authentication token expired")})
	case "revoked":
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": s.tr(locale, "Authentication token revoked")})
	default:
		s.internalError(w, r, errors.New(failure.detail))
	}
}

type authedHandler func(w http.ResponseWriter, r *http.Request, auth *authResult)

// requireAuth is jwt_required().
func (s *Server) requireAuth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, failure := s.authenticate(r)
		if failure != nil {
			s.authFailureResponse(w, r, failure)
			return
		}
		next(w, r, auth)
	}
}

// requireAdmin is admin_required: the database role, not the token claim, decides.
func (s *Server) requireAdmin(next authedHandler) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request, auth *authResult) {
		if auth.user.Role != "admin" {
			s.jsonError(w, r, http.StatusForbidden, "Administrator access required")
			return
		}
		next(w, r, auth)
	})
}

// refreshSessionCookie re-issues a cookie token in its last ten minutes, but
// only while it is exactly current, keeping its version, session id and session
// start; it never extends a session past SESSION_MAX_HOURS from sign-in.
func (s *Server) refreshSessionCookie(r *http.Request, h http.Header) {
	if responseSetsAccessCookie(h) {
		return
	}
	cookie, err := r.Cookie(accessCookieName)
	if err != nil || cookie.Value == "" {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			s.logger.Error("Session cookie refresh failed", "panic", fmt.Sprint(recovered))
		}
	}()
	auth, failure := s.authenticate(r)
	if failure != nil {
		if failure.kind == "error" {
			s.logger.Error("Session cookie refresh failed", "error", failure.detail)
		}
		return
	}
	if auth.location != "cookies" || auth.claims.sub == "" || auth.claims.exp == 0 ||
		!time.Unix(auth.claims.exp, 0).Before(s.now().Add(10*time.Minute)) {
		return
	}
	started, ok := auth.claims.sessionStarted()
	if !ok || float64(started)+s.sessionMax.Seconds() <= float64(auth.claims.exp) {
		return
	}
	// Re-check from one fresh read: a change committed after it makes the new
	// token carry an old version or a revoked session, rejected on next use.
	user, err := s.currentTokenUser(r.Context(), s.db.conn(), auth.claims)
	if err != nil {
		s.logger.Error("Session cookie refresh failed", "error", err)
		return
	}
	if user == nil {
		return
	}
	token, csrf := s.issueToken(user, auth.claims.sessionID(), started)
	s.setAccessCookies(h, token, csrf)
}
