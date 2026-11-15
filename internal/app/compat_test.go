package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// Golden values produced by the Python application's libraries
// (cryptography.fernet, argon2-cffi, pyotp, hmac, json) so data written by it
// keeps working here.
const (
	pythonFernetKey   = "LWVRuulBYzqj-8aKCjpIcLZ_zFN5eFfiUoKQZS8LwjI="
	pythonFernetToken = "gAAAAABq-P_z0uziUWDmQAa-hIPtmsBGXSmXqybpMF9RYLt5tUGK5pUrZcJfLvJq-tn-pn1a4w1RWsNdLqFMxBddpLF8V7OVdijYuxvMNZMpF7m4bRjUwaw="
	pythonArgonHash   = "$argon2id$v=19$m=65536,t=3,p=4$qm3Il7FyKMVBWTXzunAhrA$/Gt0VCXwIWfLqmeSVY9oVWpXudIlfw5q183yPqReQjM"
	pythonMFAKey      = "4bdaa63129688b2e234488206ebddc201a53d3b982e5d0395d450942fd9dfb7c"
)

func TestEncryptionKeyDerivationMatchesPython(t *testing.T) {
	key, err := encryptionKey("", testJWTSecret)
	if err != nil || key != pythonFernetKey {
		t.Fatalf("derived %q (%v), want %q", key, err, pythonFernetKey)
	}
	explicit, err := encryptionKey(pythonFernetKey, "ignored")
	if err != nil || explicit != pythonFernetKey {
		t.Fatalf("explicit key not kept: %q %v", explicit, err)
	}
	if _, err := encryptionKey("not-a-fernet-key", "x"); err == nil {
		t.Fatal("invalid explicit key accepted")
	}
}

func TestFernetReadsPythonTokensAndRoundTrips(t *testing.T) {
	cipher, err := newFernet(pythonFernetKey)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := cipher.decrypt(pythonFernetToken, 0, time.Now())
	if err != nil || string(plain) != "sk-ant-secret-value" {
		t.Fatalf("decrypt python token: %q %v", plain, err)
	}
	token := cipher.encrypt([]byte("value"), time.Now())
	if plain, err := cipher.decrypt(token, time.Minute, time.Now()); err != nil || string(plain) != "value" {
		t.Fatalf("round trip: %q %v", plain, err)
	}
	if _, err := cipher.decrypt(token, time.Minute, time.Now().Add(2*time.Minute)); err == nil {
		t.Fatal("expired token accepted with ttl")
	}
	if _, err := cipher.decrypt(token, time.Minute, time.Now().Add(-2*time.Minute)); err == nil {
		t.Fatal("token from the future accepted with ttl")
	}
	tampered := []byte(token)
	tampered[20] ^= 1
	if _, err := cipher.decrypt(string(tampered), 0, time.Now()); err == nil {
		t.Fatal("tampered token accepted")
	}
	other, _ := newFernet("Wr3qwx3-v1TS8Dgjb0LZ3XWv1i1uF4tVu09SVhBwdNc=")
	if _, err := other.decrypt(token, 0, time.Now()); err == nil {
		t.Fatal("token decrypted with another key")
	}
}

func TestArgon2VerifiesPythonHashesAndProducesCompatibleOnes(t *testing.T) {
	if err := verifyPassword(pythonArgonHash, "correct horse battery"); err != nil {
		t.Fatalf("python hash rejected: %v", err)
	}
	if err := verifyPassword(pythonArgonHash, "wrong password!"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if passwordNeedsRehash(pythonArgonHash) {
		t.Fatal("default-parameter hash marked for rehash")
	}
	hash := hashPassword("another password")
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=4$") || verifyPassword(hash, "another password") != nil {
		t.Fatalf("bad hash %q", hash)
	}
	weaker := "$argon2id$v=19$m=8192,t=2,p=1$qm3Il7FyKMVBWTXzunAhrA$/Gt0VCXwIWfLqmeSVY9oVWpXudIlfw5q183yPqReQjM"
	if !passwordNeedsRehash(weaker) {
		t.Fatal("weaker parameters not marked for rehash")
	}
	for _, invalid := range []string{"", "plaintext", "$argon2d$v=19$m=65536,t=3,p=4$AAAA$AAAA",
		"$argon2id$v=19$m=999999999,t=3,p=4$qm3Il7FyKMVBWTXzunAhrA$AAAAAAAA"} {
		if verifyPassword(invalid, "x") == nil {
			t.Fatalf("invalid hash %q verified", invalid)
		}
	}
}

func TestTOTPAndProvisioningURIMatchPyotp(t *testing.T) {
	key, err := decodeBase32Secret("JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if code := totpAt(key, 1700000000/30); code != "324550" {
		t.Fatalf("totp %s, want 324550", code)
	}
	want := "otpauth://totp/Subtitle%20Translator:alice?secret=JBSWY3DPEHPK3PXP&issuer=Subtitle%20Translator"
	if uri := provisioningURI("JBSWY3DPEHPK3PXP", "alice"); uri != want {
		t.Fatalf("uri %s", uri)
	}
	if secret := randomBase32Secret(); len(secret) != 32 || strings.Trim(secret, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
		t.Fatalf("bad secret %q", secret)
	}
	mac := hmac.New(sha256.New, []byte(testJWTSecret))
	mac.Write([]byte("subtitle-mfa-v1"))
	if hex.EncodeToString(mac.Sum(nil)) != pythonMFAKey {
		t.Fatal("MFA key derivation differs from Python")
	}
}

func TestPythonFormattingHelpers(t *testing.T) {
	got := pythonJSONStrings([]string{"字幕 第1話.zh.tw.srt", `a"b.srt`, "😀.srt"})
	want := `["\u5b57\u5e55 \u7b2c1\u8a71.zh.tw.srt", "a\"b.srt", "\ud83d\ude00.srt"]`
	if got != want {
		t.Fatalf("json.dumps mismatch:\n%s\n%s", got, want)
	}
	for value, want := range map[float64]string{
		16: "16.0", 16.5: "16.5", 1e16: "1e+16", 0.0001: "0.0001", 1e-05: "1e-05", 1234567: "1234567.0",
	} {
		if got := pythonFloatRepr(value); got != want {
			t.Errorf("repr(%v) = %s, want %s", value, got, want)
		}
	}
	if roundHalfEven(2.5) != 2 || roundHalfEven(3.5) != 4 {
		t.Fatal("round is not half-to-even")
	}
	if got := recoveryHash("ABCDEF-123456"); got != recoveryHash("abcdef123456") {
		t.Fatalf("recovery normalization: %s", got)
	}
}

func TestUploadDisplayNamesMatchPythonCases(t *testing.T) {
	cases := map[string]string{
		"字幕.srt":                     "字幕.srt",
		"Фильм.vtt":                  "Фильм.vtt",
		"映画 第1話.ass":                 "映画 第1話.ass",
		"Ame\u0301lie.srt":           "Am\u00e9lie.srt",
		"Movie.SRT":                  "Movie.SRT",
		"../../etc/passwd.srt":       "passwd.srt",
		"C:\\Users\\me\\episode.ssa": "episode.ssa",
		"a\"b'c<d>e:f|g?h*.srt":      "abcdefgh.srt",
		".hidden.srt":                "hidden.srt",
		"\u202eevil\x00\x07.srt":     "evil.srt",
		"tab\there\u3000wide.srt":    "tabhere wide.srt",
		"..srt":                      "subtitle.srt",
		"CON.srt":                    "_CON.srt",
		"notes.txt":                  "",
		"字幕":                         "",
		"":                           "",
	}
	for raw, want := range cases {
		if got := uploadDisplayName(raw); got != want {
			t.Errorf("uploadDisplayName(%q) = %q, want %q", raw, got, want)
		}
	}
	long := uploadDisplayName(strings.Repeat("字", 300) + ".srt")
	if !strings.HasSuffix(long, ".srt") || len(strings.TrimSuffix(long, ".srt")) > maxFilenameStemBytes {
		t.Fatalf("long name not capped: %d bytes", len(long))
	}
}

func TestContentDispositionMatchesWerkzeug(t *testing.T) {
	if got := contentDisposition("movie.zh.tw.srt"); got != "attachment; filename=movie.zh.tw.srt" {
		t.Fatalf("ascii: %s", got)
	}
	got := contentDisposition("字幕 第1話.zh.tw.srt")
	if !strings.Contains(got, "filename*=UTF-8''%E5%AD%97%E5%B9%95%20%E7%AC%AC1%E8%A9%B1.zh.tw.srt") {
		t.Fatalf("unicode: %s", got)
	}
	if got := contentDisposition(`a "b".srt`); got != `attachment; filename="a \"b\".srt"` {
		t.Fatalf("quoted: %s", got)
	}
}

func TestParseTimestampAcceptsPythonIsoformat(t *testing.T) {
	for _, value := range []string{"2026-11-15T08:00:00+00:00", "2026-11-15T08:00:00", "2026-11-15T08:00:00Z",
		"2026-11-15 08:00:00+00:00", "2026-11-15T08:00:00.123456+00:00", "2026-11-15T16:00:00+08:00"} {
		parsed, ok := parseTimestamp(value)
		if !ok || !parsed.Truncate(time.Second).Equal(time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)) {
			t.Errorf("parseTimestamp(%q) = %v %v", value, parsed, ok)
		}
	}
	if _, ok := parseTimestamp("garbage"); ok {
		t.Fatal("garbage parsed")
	}
	if got := isoSeconds(time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("x", 3600))); got != "2026-01-02T02:04:05+00:00" {
		t.Fatalf("isoSeconds: %s", got)
	}
}

func TestCalendarQuotaWindows(t *testing.T) {
	moment := time.Date(2026, 11, 15, 13, 0, 0, 0, time.UTC) // a Sunday
	start, reset := calendarQuotaWindow("weekly", moment)
	if !start.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) || !reset.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly %v %v", start, reset)
	}
	start, reset = calendarQuotaWindow("monthly", time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC))
	if !start.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) || !reset.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly %v %v", start, reset)
	}
}

func TestLocaleNegotiationAndFormatting(t *testing.T) {
	catalogs, err := loadCatalogs(testLocales())
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[string]string{"zh-Hant-HK": "zh-TW", "zh": "zh-CN", "zh_Hans": "zh-CN", "EN-us": "en",
		"fr": ""} {
		if got := catalogs.normalize(tag); got != want {
			t.Errorf("normalize(%s) = %q, want %q", tag, got, want)
		}
	}
	if got := acceptedLanguages([]string{"fr;q=0.9, zh-TW;q=0.95, en;q=0"}); strings.Join(got, ",") != "zh-TW,fr" {
		t.Fatalf("accept-language order %v", got)
	}
	if got := formatFields("{a} {{b}} {c}", map[string]string{"a": "1"}); got != "1 {b} {c}" {
		t.Fatalf("format %q", got)
	}
}

func TestPythonStrOfSettingValues(t *testing.T) {
	cases := map[string]string{"16": "16", "16.0": "16.0", "1e2": "100.0", "-0": "0"}
	for literal, want := range cases {
		got, ok := pythonStr(jsonNumber(literal))
		if !ok || got != want {
			t.Errorf("str(%s) = %q, want %q", literal, got, want)
		}
	}
	if _, ok := pythonStr(true); ok {
		t.Fatal("bool accepted")
	}
	if _, ok := pythonStr(nil); ok {
		t.Fatal("null accepted")
	}
}
