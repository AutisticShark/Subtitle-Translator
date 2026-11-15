package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CAPTCHA is a server-side security boundary (rule 14): every required token is
// posted to the provider's fixed Siteverify endpoint, and missing configuration
// or any provider error fails closed.
var captchaVerifyURLs = map[string]string{
	"turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	"recaptcha": "https://www.google.com/recaptcha/api/siteverify",
	"hcaptcha":  "https://api.hcaptcha.com/siteverify",
}

const (
	captchaMaxTokenLength = 8192
	captchaTimeout        = 8 * time.Second
)

// captchaFailure is a translated message with its HTTP status.
type captchaFailure struct {
	message string
	status  int
}

// verifyCaptchaToken mirrors captcha.verify_token. The expected hostname comes
// only from the configured value, never from Host or X-Forwarded-Host.
func (s *Server) verifyCaptchaToken(ctx context.Context, locale, provider, action string, token any,
	siteKey, secretKey, configuredHostname, remoteAddr string) *captchaFailure {
	siteKey = strings.TrimSpace(siteKey)
	secretKey = strings.TrimSpace(secretKey)
	unavailable := &captchaFailure{s.tr(locale, "CAPTCHA is temporarily unavailable"), http.StatusServiceUnavailable}
	if siteKey == "" || secretKey == "" {
		s.logger.Error("CAPTCHA provider is active but is missing a site or secret key", "provider", provider)
		return unavailable
	}
	text, ok := token.(string)
	if !ok || pythonStrip(text) == "" {
		return &captchaFailure{s.tr(locale, "Complete the CAPTCHA challenge"), http.StatusBadRequest}
	}
	text = pythonStrip(text)
	failed := &captchaFailure{s.tr(locale, "CAPTCHA verification failed; please try again"), http.StatusBadRequest}
	if runeCount(text) > captchaMaxTokenLength {
		return failed
	}
	form := url.Values{"secret": {secretKey}, "response": {text}}
	if remoteAddr != "" {
		form.Set("remoteip", remoteAddr)
	}
	if provider == "hcaptcha" {
		form.Set("sitekey", siteKey)
	}
	ctx, cancel := context.WithTimeout(ctx, captchaTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, captchaVerifyURLs[provider],
		strings.NewReader(form.Encode()))
	if err != nil {
		return unavailable
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "Subtitle-Translator CAPTCHA verifier")
	response, err := s.cfg.CaptchaClient.Do(request)
	if err != nil {
		s.logger.Warn("CAPTCHA verification service error", "provider", provider, "error_type", "transport")
		return unavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		s.logger.Warn("CAPTCHA verification service error", "provider", provider, "status", response.StatusCode)
		return unavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil {
		s.logger.Warn("CAPTCHA verification service error", "provider", provider, "error_type", "read")
		return unavailable
	}
	var result any
	if err := json.Unmarshal(body, &result); err != nil {
		s.logger.Warn("CAPTCHA verification service error", "provider", provider, "error_type", "json")
		return unavailable
	}
	object, isObject := result.(map[string]any)
	if success, _ := object["success"].(bool); !isObject || !success {
		var codes any = []any{}
		if isObject && object["error-codes"] != nil {
			codes = object["error-codes"]
		}
		s.logger.Info("CAPTCHA rejected a token", "provider", provider, "error_codes", codes)
		return failed
	}
	expected := normalizeHostname(configuredHostname)
	if expected != "" {
		actual := ""
		if value, ok := object["hostname"]; ok && value != nil {
			actual = normalizeHostname(pyRepr(value))
		}
		if actual != expected {
			s.logger.Warn("CAPTCHA returned an unexpected hostname", "provider", provider)
			return failed
		}
	} else if _, warned := s.warnOnce.LoadOrStore("captcha-hostname", true); !warned {
		// Legacy configurations may predate the required hostname.
		s.logger.Warn("CAPTCHA hostname is not configured; skipping the hostname check. "+
			"Set captcha_hostname to this site's public hostname.", "provider", provider)
	}
	if provider == "turnstile" {
		if value, ok := object["action"].(string); !ok || value != action {
			s.logger.Warn("Turnstile CAPTCHA returned an unexpected action")
			return failed
		}
	}
	return nil
}

func pyRepr(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// verifyCaptcha checks a required CAPTCHA for action; nil means allowed.
func (s *Server) verifyCaptcha(ctx context.Context, r *http.Request, action string, token any) (*captchaFailure, error) {
	settings, err := s.readSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !captchaRequired(action, settings) {
		return nil, nil
	}
	provider := settings.get("captcha_provider")
	settings, err = s.readSettings(ctx, provider+"_secret_key")
	if err != nil {
		// An unreadable secret cannot verify anything: fail closed.
		s.logger.Error("CAPTCHA secret key cannot be decrypted", "provider", provider)
		return &captchaFailure{s.tr(localeOf(r), "CAPTCHA is temporarily unavailable"),
			http.StatusServiceUnavailable}, nil
	}
	return s.verifyCaptchaToken(ctx, localeOf(r), provider, action, token, settings.get(provider+"_site_key"),
		settings.secrets[provider+"_secret_key"], settings.get("captcha_hostname"), state(r).remote), nil
}
