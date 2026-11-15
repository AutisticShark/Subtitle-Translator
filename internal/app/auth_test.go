package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func jsonNumber(value string) json.Number { return json.Number(value) }

func TestHealthIsPublicAndAPIRequiresJWT(t *testing.T) {
	env := newEnv(t)
	anonymous := env.client()
	expectStatus(t, anonymous.get("/healthz"), http.StatusOK)
	result := anonymous.get("/api/jobs")
	expectStatus(t, result, http.StatusUnauthorized)
	if result.str("error") != "Authentication required" {
		t.Fatalf("error %q", result.str("error"))
	}
	if result.header.Get("X-Frame-Options") != "DENY" || result.header.Get("Cache-Control") != "no-store" ||
		!strings.Contains(result.header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("security headers missing: %v", result.header)
	}
	status := anonymous.get("/api/auth/setup-status").json()
	if status["configured"] != false || status["registration_enabled"] != true {
		t.Fatalf("setup-status %v", status)
	}
}

func TestSetupIsOneTimeAndConcurrentSetupsCreateOneAdmin(t *testing.T) {
	env := newEnv(t)
	var wg sync.WaitGroup
	statuses := make([]int, 8)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = env.client().post("/api/auth/setup", map[string]any{
				"username": "admin" + string(rune('a'+i)), "password": testPassword,
			}).status
		}()
	}
	wg.Wait()
	created := 0
	for _, status := range statuses {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	if created != 1 || env.scalar("SELECT COUNT(*) FROM users") != 1 {
		t.Fatalf("created %d admins", created)
	}
	expectStatus(t, env.client().post("/api/auth/setup", map[string]any{"username": "late", "password": testPassword}),
		http.StatusConflict)
}

func TestBootstrapAdminClaimsLegacyJobs(t *testing.T) {
	env := newEnv(t, func(c *Config) { c.AdminPassword = testPassword; c.AdminUsername = "Root" })
	if env.text("SELECT role FROM users WHERE username = 'root'") != "admin" {
		t.Fatal("bootstrap admin missing")
	}
	env.login("root")
	// Restarting with another bootstrap password never creates a second admin.
	env.restart(func(c *Config) { c.AdminPassword = "different password 1"; c.AdminUsername = "x" })
	if env.scalar("SELECT COUNT(*) FROM users") != 1 {
		t.Fatal("bootstrap repeated")
	}
}

func TestCookieSessionNeedsCSRFAndLogoutRevokesTheSession(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	if admin.cookie(accessCookieName) == "" || admin.cookie(csrfCookieName) == "" {
		t.Fatal("setup did not set session cookies")
	}
	expectStatus(t, admin.get("/api/auth/me"), http.StatusOK)
	// The same token without the CSRF header is refused for unsafe methods.
	stolen := env.client()
	stolen.setCookie(accessCookieName, admin.cookie(accessCookieName))
	missing := stolen.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": "dark"})
	expectStatus(t, missing, http.StatusUnauthorized)
	if missing.str("detail") != "Missing CSRF token" {
		t.Fatalf("detail %q", missing.str("detail"))
	}
	stolen.header[csrfHeaderName] = "wrong"
	expectStatus(t, stolen.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": "dark"}),
		http.StatusUnauthorized)
	delete(stolen.header, csrfHeaderName)

	// A second session of the same account survives the first one's logout.
	other := env.login("admin")
	token := admin.cookie(accessCookieName)
	expectStatus(t, admin.post("/api/auth/logout", map[string]any{}), http.StatusOK)
	if admin.cookie(accessCookieName) != "" {
		t.Fatal("logout kept the cookie")
	}
	replay := env.client()
	replay.bearer = token
	result := replay.get("/api/auth/me")
	expectStatus(t, result, http.StatusUnauthorized)
	if result.str("error") != "Authentication token revoked" {
		t.Fatalf("error %q", result.str("error"))
	}
	expectStatus(t, other.get("/api/auth/me"), http.StatusOK)
}

func TestBearerTransportSkipsCSRFAndReturnsAToken(t *testing.T) {
	env := newEnv(t)
	env.setupAdmin()
	api := env.client()
	result := api.post("/api/auth/login", map[string]any{
		"username": "admin", "password": testPassword, "token_transport": "header",
	})
	expectStatus(t, result, http.StatusOK)
	token := result.str("access_token")
	if token == "" || api.cookie(accessCookieName) != "" {
		t.Fatalf("header transport: token %q, cookie %q", token, api.cookie(accessCookieName))
	}
	bearer := env.client()
	bearer.bearer = token
	expectStatus(t, bearer.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": "dark"}), http.StatusOK)
	bad := env.client()
	bad.header["Authorization"] = "Bearer " + token + " extra"
	expectStatus(t, bad.get("/api/auth/me"), http.StatusUnauthorized)
}

func TestTokensAreCompatibleWithFlaskJWTExtendedClaims(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(admin.cookie(accessCookieName), claims, func(*jwt.Token) (any, error) {
		return []byte(testJWTSecret), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fresh", "iat", "jti", "type", "sub", "nbf", "csrf", "exp", "role", "ver", "sid", "sst"} {
		if _, ok := claims[name]; !ok {
			t.Errorf("claim %s missing", name)
		}
	}
	if claims["type"] != "access" || claims["csrf"] != admin.cookie(csrfCookieName) || claims["fresh"] != true {
		t.Fatalf("claims %v", claims)
	}
	// A token signed like Flask-JWT-Extended without sid/sst (older releases)
	// is accepted, and alg=none or another key is not.
	user := claims["sub"].(string)
	legacy, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"fresh": false, "iat": time.Now().Unix(), "jti": "legacy-jti", "type": "access", "sub": user,
		"nbf": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "role": "admin", "ver": 0,
	}).SignedString([]byte(testJWTSecret))
	legacyClient := env.client()
	legacyClient.bearer = legacy
	expectStatus(t, legacyClient.get("/api/auth/me"), http.StatusOK)
	forged, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": user, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "ver": 0, "jti": "x",
	}).SignedString([]byte("another secret"))
	legacyClient.bearer = forged
	expectStatus(t, legacyClient.get("/api/auth/me"), http.StatusUnauthorized)
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{
		"sub": user, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "ver": 0, "jti": "y",
	}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	legacyClient.bearer = none
	expectStatus(t, legacyClient.get("/api/auth/me"), http.StatusUnauthorized)
	// Logging out the legacy token revokes it by its jti.
	legacyClient.bearer = legacy
	expectStatus(t, legacyClient.post("/api/auth/logout", map[string]any{}), http.StatusOK)
	expectStatus(t, legacyClient.get("/api/auth/me"), http.StatusUnauthorized)
}

func TestRefreshKeepsSessionAndStopsAtTheAbsoluteLifetime(t *testing.T) {
	env := newEnv(t, func(c *Config) { c.SessionMaxHours = 1; c.JWTAccessMinutes = 30 })
	admin := env.setupAdmin()
	parse := func(token string) jwt.MapClaims {
		claims := jwt.MapClaims{}
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		if _, err := parser.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) {
			return []byte(testJWTSecret), nil
		}); err != nil {
			t.Fatal(err)
		}
		return claims
	}
	first := parse(admin.cookie(accessCookieName))
	// Not yet near expiry: no refresh.
	admin.get("/api/auth/me")
	if parse(admin.cookie(accessCookieName))["jti"] != first["jti"] {
		t.Fatal("refreshed too early")
	}
	env.clock.advance(25 * time.Minute)
	expectStatus(t, admin.get("/api/auth/me"), http.StatusOK)
	second := parse(admin.cookie(accessCookieName))
	if second["jti"] == first["jti"] || second["sid"] != first["sid"] || second["sst"] != first["sst"] {
		t.Fatalf("refresh changed the session: %v -> %v", first, second)
	}
	// The second token is capped at sst + 1h.
	exp, _ := second.GetExpirationTime()
	sst := int64(first["sst"].(float64))
	if exp.Unix() > sst+3600 {
		t.Fatalf("token outlives the session: exp %d sst %d", exp.Unix(), sst)
	}
	env.clock.advance(30 * time.Minute) // 55 minutes in: the token ends at the deadline
	admin.get("/api/auth/me")
	third := parse(admin.cookie(accessCookieName))
	if exp3, _ := third.GetExpirationTime(); exp3.Unix() > sst+3600 {
		t.Fatal("refresh extended the session")
	}
	env.clock.advance(10 * time.Minute)
	expectStatus(t, admin.get("/api/auth/me"), http.StatusUnauthorized)
}

func TestRefreshNeverRevivesARevokedOrChangedSession(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	env.clock.advance(25 * time.Minute)
	var id string
	for _, entry := range admin.get("/api/users").json()["users"].([]any) {
		if row := entry.(map[string]any); row["username"] == "member" {
			id = row["id"].(string)
		}
	}
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+id, map[string]any{"password": "brand new password"}),
		http.StatusOK)
	before := user.cookie(accessCookieName)
	expectStatus(t, user.get("/api/auth/me"), http.StatusUnauthorized)
	if after := user.cookie(accessCookieName); after != before {
		t.Fatal("a token with an old version was refreshed")
	}
}

func TestLoginLockoutAndConcurrentGuesses(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	env.createUser(admin, "target", "user")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			env.client().post("/api/auth/login", map[string]any{"username": "target", "password": "wrong password!!"})
		}()
	}
	wg.Wait()
	// Parallel guesses could not exceed the budget: the account is now locked,
	// and even the right password fails until the lock expires.
	if env.text("SELECT locked_until FROM users WHERE username = 'target'") == "" {
		t.Fatal("account not locked after parallel failures")
	}
	expectStatus(t, env.client().post("/api/auth/login", map[string]any{"username": "target", "password": testPassword}),
		http.StatusUnauthorized)
	env.clock.advance(16 * time.Minute)
	env.login("target")
	if env.scalar("SELECT failed_login_count FROM users WHERE username = 'target'") != 0 {
		t.Fatal("success did not reset the counter")
	}
}

func TestAddressLimiterReservesAtomicallyAndPrunes(t *testing.T) {
	clock := &testClock{}
	limiter := newAttemptLimiter(3, time.Minute, clock.now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := limiter.reserve("1.2.3.4"); ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 3 || !limiter.limited("1.2.3.4") {
		t.Fatalf("granted %d", granted)
	}
	marker, _ := limiter.reserve("5.6.7.8")
	limiter.release("5.6.7.8", marker)
	if _, ok := limiter.attempts["5.6.7.8"]; ok {
		t.Fatal("release kept the address")
	}
	clock.advance(2 * time.Minute)
	limiter.reserve("9.9.9.9")
	if _, ok := limiter.attempts["1.2.3.4"]; ok {
		t.Fatal("expired address not pruned")
	}
}

func TestForwardedHeadersAreTrustedOnlyWithAProxyCount(t *testing.T) {
	env := newEnv(t)
	request, _ := http.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.1:5555"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")
	if got := env.server.remoteAddr(request); got != "10.0.0.1" {
		t.Fatalf("untrusted header used: %s", got)
	}
	env.server.cfg.TrustedProxyCount = 1
	if got := env.server.remoteAddr(request); got != "198.51.100.7" {
		t.Fatalf("one proxy: %s", got)
	}
	env.server.cfg.TrustedProxyCount = 3
	if got := env.server.remoteAddr(request); got != "10.0.0.1" {
		t.Fatalf("too few entries: %s", got)
	}
}

func TestRegistrationCreatesRegularUsersAndCanBeDisabled(t *testing.T) {
	env := newEnv(t)
	expectStatus(t, env.client().post("/api/auth/register", map[string]any{
		"username": "early", "password": testPassword, "confirm_password": testPassword,
	}), http.StatusConflict)
	admin := env.setupAdmin()
	user := env.client()
	result := user.post("/api/auth/register", map[string]any{
		"username": "Newbie", "password": testPassword, "confirm_password": testPassword,
	})
	expectStatus(t, result, http.StatusCreated)
	if result.json()["user"].(map[string]any)["role"] != "user" || user.cookie(accessCookieName) == "" {
		t.Fatalf("registration %s", result.body)
	}
	expectStatus(t, env.client().post("/api/auth/register", map[string]any{
		"username": "newbie", "password": testPassword, "confirm_password": testPassword,
	}), http.StatusConflict)
	expectStatus(t, env.client().post("/api/auth/register", map[string]any{
		"username": "other", "password": testPassword, "confirm_password": "different value!",
	}), http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"registration_enabled": "0"}),
		http.StatusOK)
	expectStatus(t, env.client().post("/api/auth/register", map[string]any{
		"username": "blocked", "password": testPassword, "confirm_password": testPassword,
	}), http.StatusForbidden)
}

func TestAuthBodiesAreCapped(t *testing.T) {
	env := newEnv(t)
	huge := strings.Repeat("x", authBodyLimit)
	result := env.client().do(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"`+huge+`"}`),
		"application/json")
	expectStatus(t, result, http.StatusRequestEntityTooLarge)
	if result.str("error") != "Authentication request is too large" {
		t.Fatalf("error %q", result.str("error"))
	}
}

func TestAdministratorRosterProtection(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	adminID := admin.get("/api/auth/me").json()["user"].(map[string]any)["id"].(string)
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+adminID, map[string]any{"role": "user"}),
		http.StatusConflict)
	expectStatus(t, admin.do(http.MethodDelete, "/api/users/"+adminID, nil, ""), http.StatusConflict)
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+adminID, map[string]any{"bogus": 1}),
		http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+adminID, map[string]any{"active": "no"}),
		http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+adminID, map[string]any{"unlock": "yes"}),
		http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+adminID, map[string]any{"role": []any{"admin"}}),
		http.StatusBadRequest)

	// Two administrators demoting each other at once leave one active admin.
	second := env.createUser(admin, "second", "admin")
	secondID := second.get("/api/auth/me").json()["user"].(map[string]any)["id"].(string)
	var wg sync.WaitGroup
	results := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = admin.send(http.MethodPatch, "/api/users/"+secondID, map[string]any{"role": "user"}).status
	}()
	go func() {
		defer wg.Done()
		results[1] = second.send(http.MethodPatch, "/api/users/"+adminID, map[string]any{"role": "user"}).status
	}()
	wg.Wait()
	if count := env.scalar("SELECT COUNT(*) FROM users WHERE role = 'admin' AND active = ?", true); count != 1 {
		t.Fatalf("%d active admins after mutual demotion (%v)", count, results)
	}
}

func TestDeactivationRevokesTokensAndRegularUsersAreDeniedAdminAPIs(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	expectStatus(t, user.get("/api/users"), http.StatusForbidden)
	expectStatus(t, user.send(http.MethodPut, "/api/settings", map[string]any{"rpm": "1"}), http.StatusForbidden)
	userID := user.get("/api/auth/me").json()["user"].(map[string]any)["id"].(string)
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+userID, map[string]any{"active": false}), http.StatusOK)
	expectStatus(t, user.get("/api/auth/me"), http.StatusUnauthorized)
	users := admin.get("/api/users").json()["users"].([]any)
	if len(users) != 2 || users[1].(map[string]any)["job_count"] != float64(0) {
		t.Fatalf("users %v", users)
	}
	expectStatus(t, admin.do(http.MethodDelete, "/api/users/"+userID, nil, ""), http.StatusOK)
	expectStatus(t, admin.do(http.MethodDelete, "/api/users/"+userID, nil, ""), http.StatusNotFound)
}

func TestThemePreferenceIsValidatedAndRenderedWithoutFlash(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	expectStatus(t, admin.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": "neon"}), http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": []any{}}), http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": "dark", "x": 1}),
		http.StatusBadRequest)
	expectStatus(t, admin.send(http.MethodPatch, "/api/auth/me", map[string]any{"theme": "dark"}), http.StatusOK)
	page := admin.get("/?lang=zh-TW")
	expectStatus(t, page, http.StatusOK)
	html := string(page.body)
	for _, want := range []string{`lang="zh-TW"`, `data-theme="dark"`, `id="app-boot"`, `"theme":"dark"`,
		`<script type="module" crossorigin src="/assets/index-abc.js">`} {
		if !strings.Contains(html, want) {
			t.Fatalf("index lacks %s:\n%s", want, html)
		}
	}
	if admin.cookie(localeCookie) != "zh-TW" {
		t.Fatal("explicit ?lang= not persisted")
	}
	anonymous := env.client().get("/")
	if !strings.Contains(string(anonymous.body), `data-theme="system"`) {
		t.Fatal("signed-out view is not system themed")
	}
	asset := env.client().get("/assets/index-abc.js")
	expectStatus(t, asset, http.StatusOK)
	if !strings.Contains(asset.header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset caching %q", asset.header.Get("Cache-Control"))
	}
	expectStatus(t, env.client().get("/api/nope"), http.StatusNotFound)
}

func TestRenderIndexEscapesBootData(t *testing.T) {
	page, err := renderIndex(testIndex, bootData{Locale: `en"><script>`, Theme: "dark",
		Messages: map[string]string{"x": "</script><script>alert(1)</script>"}}, "A & <B>")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	if strings.Contains(html, "</script><script>alert") || strings.Contains(html, `"><script>`) {
		t.Fatalf("unescaped injection:\n%s", html)
	}
	if !strings.Contains(html, "<title>A &amp; &lt;B&gt;</title>") {
		t.Fatalf("title not escaped:\n%s", html)
	}
}

func TestAPIErrorsFollowTheRequestLocale(t *testing.T) {
	env := newEnv(t)
	anonymous := env.client()
	anonymous.header["Accept-Language"] = "zh-TW,zh;q=0.9"
	result := anonymous.get("/api/jobs")
	if result.str("error") == "Authentication required" || result.header.Get("Content-Language") != "zh-TW" {
		t.Fatalf("not localized: %s %v", result.body, result.header.Get("Content-Language"))
	}
	catalog := env.client().get("/api/i18n?lang=zh-CN").json()
	if catalog["locale"] != "zh-CN" || len(catalog["messages"].(map[string]any)) == 0 {
		t.Fatalf("catalog %v", catalog["locale"])
	}
	if catalog["languages"].(map[string]any)["ja"] == "Japanese" {
		t.Fatal("language names not localized")
	}
}
