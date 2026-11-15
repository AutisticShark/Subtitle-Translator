package app

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEmail records sent messages or fails on demand.
type fakeEmail struct {
	mu       sync.Mutex
	messages []string
	fail     bool
}

func (f *fakeEmail) Send(_ context.Context, to, subject, text, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("email delivery failed")
	}
	f.messages = append(f.messages, to+"|"+subject+"|"+text)
	return nil
}

var codePattern = regexp.MustCompile(`\b[0-9]{6}\b`)

func (f *fakeEmail) lastCode(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) == 0 {
		t.Fatal("no email sent")
	}
	return codePattern.FindString(f.messages[len(f.messages)-1])
}

func currentTOTP(t *testing.T, env *testEnv, secret string, offset int64) string {
	t.Helper()
	key, err := decodeBase32Secret(secret)
	if err != nil {
		t.Fatal(err)
	}
	return totpAt(key, env.server.now().Unix()/30+offset)
}

// enrollTOTP enables an authenticator app and returns its secret and recovery codes.
func enrollTOTP(t *testing.T, env *testEnv, user *client) (string, []string) {
	t.Helper()
	setup := user.post("/api/auth/mfa/setup", map[string]any{"method": "totp", "password": testPassword})
	expectStatus(t, setup, http.StatusOK)
	body := setup.json()
	secret := body["secret"].(string)
	if !strings.HasPrefix(body["qr_code"].(string), "data:image/svg+xml;base64,") ||
		!strings.Contains(body["provisioning_uri"].(string), "secret="+secret) {
		t.Fatalf("setup body %v", body)
	}
	stored := env.text("SELECT secret FROM mfa_challenges WHERE purpose = 'enroll'")
	if !strings.HasPrefix(stored, secretPrefix) {
		t.Fatal("pending TOTP secret not encrypted")
	}
	confirm := user.post("/api/auth/mfa/confirm", map[string]any{
		"challenge_token": body["challenge_token"], "code": currentTOTP(t, env, secret, 0),
	})
	expectStatus(t, confirm, http.StatusOK)
	var codes []string
	for _, code := range confirm.json()["recovery_codes"].([]any) {
		codes = append(codes, code.(string))
	}
	if len(codes) != 10 || !regexp.MustCompile(`^[0-9a-f]{6}-[0-9a-f]{6}-[0-9a-f]{6}-[0-9a-f]{6}$`).MatchString(codes[0]) {
		t.Fatalf("recovery codes %v", codes)
	}
	return secret, codes
}

func TestTOTPEnrollmentLoginReplayAndRecovery(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	before := user.cookie(accessCookieName)
	secret, codes := enrollTOTP(t, env, user)
	// Enrollment rotated the session: the old token is dead, the new cookie works.
	stale := env.client()
	stale.bearer = before
	expectStatus(t, stale.get("/api/auth/me"), http.StatusUnauthorized)
	expectStatus(t, user.get("/api/auth/me"), http.StatusOK)
	status := user.get("/api/auth/mfa").json()
	if status["method"] != "totp" || status["recovery_remaining"] != float64(10) {
		t.Fatalf("status %v", status)
	}

	// Password login now returns a challenge, never a session.
	fresh := env.client()
	result := fresh.post("/api/auth/login", map[string]any{"username": "member", "password": testPassword})
	expectStatus(t, result, http.StatusOK)
	challenge := result.json()
	if challenge["mfa_required"] != true || fresh.cookie(accessCookieName) != "" {
		t.Fatalf("login %v", challenge)
	}
	// The challenge token is not an access token.
	probe := env.client()
	probe.bearer = challenge["challenge_token"].(string)
	expectStatus(t, probe.get("/api/auth/me"), http.StatusUnauthorized)

	env.clock.advance(31 * time.Second) // a step after enrollment's
	code := currentTOTP(t, env, secret, 0)
	expectStatus(t, fresh.post("/api/auth/mfa/verify", map[string]any{
		"challenge_token": challenge["challenge_token"], "code": code}), http.StatusOK)
	if fresh.cookie(accessCookieName) == "" {
		t.Fatal("verify did not sign in")
	}
	// The same code cannot be replayed on a new challenge.
	again := env.client()
	second := again.post("/api/auth/login", map[string]any{"username": "member", "password": testPassword}).json()
	expectStatus(t, again.post("/api/auth/mfa/verify", map[string]any{
		"challenge_token": second["challenge_token"], "code": code}), http.StatusBadRequest)
	// A recovery code works once, also with the bearer transport.
	api := env.client()
	third := api.post("/api/auth/login", map[string]any{"username": "member", "password": testPassword,
		"token_transport": "header"}).json()
	verified := api.post("/api/auth/mfa/verify", map[string]any{
		"challenge_token": third["challenge_token"], "code": strings.ToUpper(codes[0])})
	expectStatus(t, verified, http.StatusOK)
	if verified.str("access_token") == "" || api.cookie(accessCookieName) != "" {
		t.Fatal("bearer transport not kept through the challenge")
	}
	fourth := env.client()
	fourthChallenge := fourth.post("/api/auth/login", map[string]any{"username": "member",
		"password": testPassword}).json()
	expectStatus(t, fourth.post("/api/auth/mfa/verify", map[string]any{
		"challenge_token": fourthChallenge["challenge_token"], "code": codes[0]}), http.StatusBadRequest)
	if stored := env.text("SELECT recovery_hashes FROM mfa_accounts"); !strings.HasPrefix(stored, `["`) ||
		strings.Count(stored, `", "`) != 8 {
		t.Fatalf("recovery hashes not stored like json.dumps: %s", stored)
	}

	// Disabling needs the password and a factor, and ends other sessions.
	expectStatus(t, user.post("/api/auth/mfa/disable", map[string]any{"password": "wrong password!", "code": codes[1]}),
		http.StatusBadRequest)
	expectStatus(t, user.post("/api/auth/mfa/disable", map[string]any{"password": testPassword, "code": "000000"}),
		http.StatusBadRequest)
	expectStatus(t, user.post("/api/auth/mfa/disable", map[string]any{"password": testPassword, "code": codes[1]}),
		http.StatusOK)
	expectStatus(t, fresh.get("/api/auth/me"), http.StatusUnauthorized)
	if user.get("/api/auth/mfa").json()["method"] != "" {
		t.Fatal("MFA still enabled")
	}
}

func TestConcurrentRecoveryRedemptionSucceedsOnce(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	_, codes := enrollTOTP(t, env, user)
	tokens := make([]string, 4)
	clients := make([]*client, 4)
	for i := range tokens {
		clients[i] = env.client()
		tokens[i] = clients[i].post("/api/auth/login", map[string]any{"username": "member",
			"password": testPassword, "token_transport": "header"}).json()["challenge_token"].(string)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := range tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := clients[i].post("/api/auth/mfa/verify", map[string]any{"challenge_token": tokens[i],
				"code": codes[0]})
			if result.status == http.StatusOK {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("recovery code redeemed %d times", successes)
	}
}

func TestMFAFailureBudgetsAreSeparateAndPersistent(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	_, codes := enrollTOTP(t, env, user)
	// Five wrong management passwords lock management, not sign-in.
	for range 5 {
		user.post("/api/auth/mfa/recovery", map[string]any{"password": "wrong password!", "code": codes[0]})
	}
	result := user.post("/api/auth/mfa/recovery", map[string]any{"password": testPassword, "code": codes[0]})
	expectStatus(t, result, http.StatusTooManyRequests)
	if result.header.Get("Retry-After") == "" {
		t.Fatal("Retry-After missing")
	}
	login := func() map[string]any {
		return env.client().post("/api/auth/login", map[string]any{"username": "member",
			"password": testPassword}).json()
	}
	challenge := login()
	if challenge["mfa_required"] != true {
		t.Fatalf("login locked by management failures: %v", challenge)
	}
	// Sign-in failures persist across new password logins and lock after five.
	for range 5 {
		next := login()
		env.client().post("/api/auth/mfa/verify", map[string]any{"challenge_token": next["challenge_token"],
			"code": "111111"})
	}
	locked := env.client().post("/api/auth/login", map[string]any{"username": "member", "password": testPassword})
	expectStatus(t, locked, http.StatusTooManyRequests)
	// An administrator unlock clears MFA locks without removing the factor.
	memberID := env.text("SELECT id FROM users WHERE username = 'member'")
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+memberID, map[string]any{"unlock": true}), http.StatusOK)
	if env.text("SELECT method FROM mfa_accounts WHERE user_id = ?", memberID) != "totp" {
		t.Fatal("unlock removed MFA")
	}
	if login()["mfa_required"] != true {
		t.Fatal("still locked after unlock")
	}
	// A password reset by an administrator keeps MFA.
	expectStatus(t, admin.send(http.MethodPatch, "/api/users/"+memberID,
		map[string]any{"password": "another password 9"}), http.StatusOK)
	if env.text("SELECT method FROM mfa_accounts WHERE user_id = ?", memberID) != "totp" {
		t.Fatal("password reset removed MFA")
	}
}

func TestUnreadableTOTPSecretKeepsRecoveryAndSpendsTheBudget(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	_, codes := enrollTOTP(t, env, user)
	env.restart(func(c *Config) { c.JWTSecret = "a rotated secret" })
	challenge := env.client().post("/api/auth/login", map[string]any{"username": "member",
		"password": testPassword}).json()
	result := env.client().post("/api/auth/mfa/verify", map[string]any{"challenge_token": challenge["challenge_token"],
		"code": "123456"})
	expectStatus(t, result, http.StatusServiceUnavailable)
	if env.scalar("SELECT failures FROM mfa_accounts") != 1 {
		t.Fatal("unreadable secret did not spend the budget")
	}
	expectStatus(t, env.client().post("/api/auth/mfa/verify", map[string]any{
		"challenge_token": challenge["challenge_token"], "code": codes[0]}), http.StatusOK)
}

func TestEmailMFAEnrollmentCooldownAndOutage(t *testing.T) {
	sender := &fakeEmail{}
	env := newEnv(t, func(c *Config) { c.Email = sender })
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	expectStatus(t, user.post("/api/auth/mfa/setup", map[string]any{"method": "email", "password": testPassword,
		"email": "not an address"}), http.StatusBadRequest)
	setup := user.post("/api/auth/mfa/setup", map[string]any{"method": "email", "password": testPassword,
		"email": "member@example.com"})
	expectStatus(t, setup, http.StatusOK)
	if setup.json()["email_sent"] != true {
		t.Fatalf("setup %s", setup.body)
	}
	code := sender.lastCode(t)
	if stored := env.text("SELECT code_hash FROM mfa_challenges"); stored == "" || strings.Contains(stored, code) {
		t.Fatal("email code not stored as an HMAC")
	}
	confirm := user.post("/api/auth/mfa/confirm", map[string]any{"challenge_token": setup.json()["challenge_token"],
		"code": code})
	expectStatus(t, confirm, http.StatusOK)
	if user.get("/api/auth/mfa").json()["email"] != "m***@example.com" {
		t.Fatal("masked email missing")
	}
	// Sign-in during the one-minute cooldown keeps the delivered code valid.
	env.clock.advance(61 * time.Second)
	first := env.client().post("/api/auth/login", map[string]any{"username": "member", "password": testPassword}).json()
	if first["email_sent"] != true {
		t.Fatalf("first login %v", first)
	}
	loginCode := sender.lastCode(t)
	second := env.client().post("/api/auth/login", map[string]any{"username": "member", "password": testPassword}).json()
	if second["email_sent"] != false || !strings.Contains(second["warning"].(string), "requested recently") {
		t.Fatalf("cooldown login %v", second)
	}
	verify := env.client()
	expectStatus(t, verify.post("/api/auth/mfa/verify", map[string]any{"challenge_token": second["challenge_token"],
		"code": loginCode}), http.StatusOK)
	// During an outage the code is forgotten and the response says so.
	sender.mu.Lock()
	sender.fail = true
	sender.mu.Unlock()
	env.clock.advance(61 * time.Second)
	failed := env.client().post("/api/auth/login", map[string]any{"username": "member", "password": testPassword}).json()
	if failed["email_sent"] != false || failed["warning"] == nil {
		t.Fatalf("outage login %v", failed)
	}
	if env.text("SELECT code_hash FROM mfa_challenges WHERE purpose = 'login'") != "" {
		t.Fatal("undelivered code kept")
	}
}

func TestMFAManagementRequiresASessionAndCSRF(t *testing.T) {
	env := newEnv(t)
	admin := env.setupAdmin()
	expectStatus(t, env.client().post("/api/auth/mfa/setup", map[string]any{"method": "totp"}), http.StatusUnauthorized)
	noCSRF := env.client()
	noCSRF.setCookie(accessCookieName, admin.cookie(accessCookieName))
	expectStatus(t, noCSRF.post("/api/auth/mfa/setup", map[string]any{"method": "totp", "password": testPassword}),
		http.StatusUnauthorized)
	expectStatus(t, env.client().post("/api/auth/mfa/verify", map[string]any{"challenge_token": "x", "code": "1"}),
		http.StatusBadRequest)
	unstable := newEnv(t, func(c *Config) { c.JWTSecret = "" })
	adminUnstable := unstable.setupAdmin()
	expectStatus(t, adminUnstable.post("/api/auth/mfa/setup", map[string]any{"method": "totp",
		"password": testPassword}), http.StatusServiceUnavailable)
}

func TestInvalidEmailConfigurationDisablesEmailWithoutStoppingTheServer(t *testing.T) {
	for _, env := range []map[string]string{
		{"EMAIL_PROVIDER": "carrier-pigeon"},
		{"SMTP_HOST": "smtp.example.com"},
		{"EMAIL_PROVIDER": "resend"},
	} {
		config, err := configFromMap(env)
		if err != nil || config.Email != nil {
			t.Fatalf("%v: config error %v, email %v", env, err, config.Email)
		}
	}
	// An account enrolled for email codes keeps signing in with a recovery code
	// when delivery is unavailable.
	sender := &fakeEmail{}
	env := newEnv(t, func(c *Config) { c.Email = sender })
	admin := env.setupAdmin()
	user := env.createUser(admin, "member", "user")
	setup := user.post("/api/auth/mfa/setup", map[string]any{"method": "email", "password": testPassword,
		"email": "member@example.com"}).json()
	confirm := user.post("/api/auth/mfa/confirm", map[string]any{"challenge_token": setup["challenge_token"],
		"code": sender.lastCode(t)})
	expectStatus(t, confirm, http.StatusOK)
	recovery := confirm.json()["recovery_codes"].([]any)[0].(string)
	env.restart(func(c *Config) { c.Email = nil })
	if env.login("admin").get("/api/auth/mfa").json()["email_available"] != false {
		t.Fatal("email reported available")
	}
	login := env.client().post("/api/auth/login", map[string]any{"username": "member", "password": testPassword}).json()
	if login["mfa_required"] != true || login["email_sent"] != false || login["warning"] == nil {
		t.Fatalf("login %v", login)
	}
	expectStatus(t, env.client().post("/api/auth/mfa/verify", map[string]any{
		"challenge_token": login["challenge_token"], "code": recovery}), http.StatusOK)
}
