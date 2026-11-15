package app

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultAnthropicModel is the anthropic_model default for new installations.
// Stored settings keep whatever an administrator saved.
const DefaultAnthropicModel = "claude-opus-5-5"

var (
	calendarPeriods  = []string{"daily", "weekly", "monthly"}
	windowLimitKeys  = []string{"rate_limit_window_minutes", "user_job_limit", "admin_job_limit", "panel_job_limit"}
	captchaProviders = []string{"turnstile", "recaptcha", "hcaptcha"}
	// captchaActions keeps the order login, register, upload.
	captchaActions = []struct{ action, setting string }{
		{"login", "captcha_on_login"}, {"register", "captcha_on_register"}, {"upload", "captcha_on_upload"},
	}
	booleanSettings = []string{"captcha_on_login", "captcha_on_register", "captcha_on_upload", "registration_enabled"}
	secretKeys      = []string{
		"anthropic_api_key", "deepl_api_key", "google_api_key", "hcaptcha_secret_key",
		"openai_api_key", "recaptcha_secret_key", "turnstile_secret_key",
	}
	publicProviders = []string{"anthropic", "openai", "deepl", "google"}
	providerLabels  = map[string]string{
		"anthropic": "Anthropic", "openai": "OpenAI-compatible", "deepl": "DeepL",
		"google": "Google Cloud Translation", "echo": "Echo (offline test)",
	}
	numberSettings = map[string][2]float64{"rpm": {0, 10000}, "width": {4, 80}}
	// integerSettings are whole numbers because the worker reads them as integers.
	integerSettings = func() map[string][2]int64 {
		bounds := map[string][2]int64{
			"batch_size": {1, 100}, "workers": {1, 16}, "max_lines": {1, 5},
			"rate_limit_window_minutes": {1, 10080}, "user_job_limit": {0, 100000},
			"admin_job_limit": {0, 100000}, "panel_job_limit": {0, 1000000},
		}
		for _, key := range calendarLimitKeys() {
			maximum := int64(100000)
			if strings.HasPrefix(key, "panel_") {
				maximum = 1000000
			}
			bounds[key] = [2]int64{0, maximum}
		}
		return bounds
	}()
	integerSettingOrder = append([]string{
		"batch_size", "workers", "max_lines", "rate_limit_window_minutes", "user_job_limit",
		"admin_job_limit", "panel_job_limit",
	}, calendarLimitKeys()...)
	hostnamePattern = regexp.MustCompile(
		`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

func calendarLimitKeys() []string {
	var keys []string
	for _, scope := range []string{"user", "admin", "panel"} {
		for _, period := range calendarPeriods {
			keys = append(keys, scope+"_"+period+"_job_limit")
		}
	}
	sort.Strings(keys)
	return keys
}

func rateLimitKeys() []string {
	keys := append(append([]string{}, windowLimitKeys...), calendarLimitKeys()...)
	sort.Strings(keys)
	return keys
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func isSecretKey(key string) bool { return contains(secretKeys, key) }

func normalizeHostname(value string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(value)), ".")
}

// settingDefaults mirrors webapp.DEFAULTS, with deployment defaults from env.
func settingDefaults(env map[string]string) (map[string]string, error) {
	get := func(name string) string { return env[name] }
	defaults := map[string]string{
		"default_provider":          "anthropic",
		"anthropic_model":           DefaultAnthropicModel,
		"openai_model":              "gpt-5-mini",
		"openai_base_url":           "https://api.openai.com/v1",
		"source_language":           "English",
		"target_languages":          "zh-TW,zh-CN",
		"batch_size":                "20",
		"workers":                   "4",
		"rpm":                       "0",
		"width":                     "16",
		"max_lines":                 "2",
		"rate_limit_window_minutes": "60",
		"user_job_limit":            "0",
		"admin_job_limit":           "0",
		"panel_job_limit":           "0",
		"registration_enabled":      strings.TrimSpace(envDefault(env, "REGISTRATION_ENABLED", "1")),
		"captcha_provider":          strings.ToLower(strings.TrimSpace(envDefault(env, "CAPTCHA_PROVIDER", "none"))),
		"captcha_hostname":          strings.ToLower(strings.TrimSpace(get("CAPTCHA_HOSTNAME"))),
		"captcha_on_login":          strings.TrimSpace(envDefault(env, "CAPTCHA_ON_LOGIN", "1")),
		"captcha_on_register":       strings.TrimSpace(envDefault(env, "CAPTCHA_ON_REGISTER", "1")),
		"captcha_on_upload":         strings.TrimSpace(envDefault(env, "CAPTCHA_ON_UPLOAD", "1")),
		"turnstile_site_key":        strings.TrimSpace(get("TURNSTILE_SITE_KEY")),
		"recaptcha_site_key":        strings.TrimSpace(get("RECAPTCHA_SITE_KEY")),
		"hcaptcha_site_key":         strings.TrimSpace(get("HCAPTCHA_SITE_KEY")),
	}
	for _, key := range calendarLimitKeys() {
		defaults[key] = "0"
	}
	if provider := defaults["captcha_provider"]; provider != "none" && !contains(captchaProviders, provider) {
		return nil, fmt.Errorf("CAPTCHA_PROVIDER must be none, turnstile, recaptcha, or hcaptcha")
	}
	for _, key := range booleanSettings {
		if value := defaults[key]; value != "0" && value != "1" {
			return nil, fmt.Errorf("%s must be 0 or 1", strings.ToUpper(key))
		}
	}
	return defaults, nil
}

// envDefault is os.environ.get(name, fallback): a blank but present variable stays blank.
func envDefault(env map[string]string, name, fallback string) string {
	if value, ok := env[name]; ok {
		return value
	}
	return fallback
}

func (s *Server) availableProviders() []string {
	if s.cfg.Debug {
		return append(append([]string{}, publicProviders...), "echo")
	}
	return publicProviders
}

// panelSettings is settings_view(): stored values merged with defaults, plus
// which providers and CAPTCHA keys are configured. Secret values appear only
// when a caller asked for them.
type panelSettings struct {
	values            map[string]string
	providers         []string
	configured        map[string]bool
	captchaConfigured map[string]bool
	secrets           map[string]string
}

func (p panelSettings) get(key string) string { return p.values[key] }

// settingsView merges rows with defaults and decrypts only the secrets listed
// in include (rule 27: one unreadable secret must not break other paths).
func (s *Server) settingsView(stored map[string]string, include ...string) (panelSettings, error) {
	result := panelSettings{values: map[string]string{}, secrets: map[string]string{}}
	for key, fallback := range s.defaults {
		if value, ok := stored[key]; ok {
			result.values[key] = value
		} else {
			result.values[key] = fallback
		}
	}
	result.providers = s.availableProviders()
	if !contains(result.providers, result.values["default_provider"]) {
		result.values["default_provider"] = s.defaults["default_provider"]
	}
	configured := map[string]bool{"echo": true}
	for _, provider := range publicProviders {
		key := provider + "_api_key"
		configured[provider] = stored[key] != "" || s.cfg.env(strings.ToUpper(key)) != ""
	}
	result.configured = map[string]bool{}
	for _, provider := range result.providers {
		result.configured[provider] = configured[provider]
	}
	result.captchaConfigured = map[string]bool{}
	for _, provider := range captchaProviders {
		secret := provider + "_secret_key"
		result.captchaConfigured[provider] = result.values[provider+"_site_key"] != "" &&
			(stored[secret] != "" || s.cfg.env(strings.ToUpper(secret)) != "")
	}
	for _, key := range include {
		if !isSecretKey(key) {
			continue
		}
		if value := stored[key]; value != "" {
			plain, err := s.decryptSecret(value)
			if err != nil {
				return result, err
			}
			result.secrets[key] = plain
		} else {
			result.secrets[key] = s.cfg.env(strings.ToUpper(key))
		}
	}
	return result, nil
}

// publicJSON is the GET /api/settings body: never any secret value.
func (p panelSettings) publicJSON(s *Server, locale string) map[string]any {
	body := make(map[string]any, len(p.values)+3)
	for key, value := range p.values {
		body[key] = value
	}
	providers := map[string]string{}
	for _, name := range p.providers {
		providers[name] = s.tr(locale, providerLabels[name])
	}
	body["providers"] = providers
	body["configured"] = p.configured
	body["captcha_configured"] = p.captchaConfigured
	return body
}

type settingRow struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func loadSettingRows(ctx context.Context, c conn) ([]settingRow, error) {
	rows, err := c.query(ctx, "SELECT name, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []settingRow
	for rows.Next() {
		var row settingRow
		if err := rows.Scan(&row.Name, &row.Value); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func rowsToMap(rows []settingRow) map[string]string {
	values := make(map[string]string, len(rows))
	for _, row := range rows {
		values[row.Name] = row.Value
	}
	return values
}

// readSettings is read_settings: cached rows, then settingsView.
func (s *Server) readSettings(ctx context.Context, include ...string) (panelSettings, error) {
	rows, err := cachedQuery(ctx, s, "settings", "all", func(c conn) ([]settingRow, error) {
		return loadSettingRows(ctx, c)
	})
	if err != nil {
		return panelSettings{}, err
	}
	return s.settingsView(rowsToMap(rows), include...)
}

// storedSettings reads inside the caller's transaction, bypassing Redis.
func (s *Server) storedSettings(ctx context.Context, c conn) (panelSettings, map[string]string, error) {
	rows, err := loadSettingRows(ctx, c)
	if err != nil {
		return panelSettings{}, nil, err
	}
	values := rowsToMap(rows)
	view, err := s.settingsView(values)
	return view, values, err
}

// lockSettings serializes settings writes with each other and with job-quota
// accounting: it is the same shared row consumeJobQuota writes first (rule 21).
func lockSettings(ctx context.Context, c conn) error {
	_, err := c.exec(ctx, "UPDATE settings SET updated_at = updated_at WHERE name = 'panel_job_limit'")
	return err
}

// validateChoiceAndFlagSettings mirrors settings_schema.validate_choice_and_flag_settings.
// clean holds the stripped text that will be stored; captcha_hostname is normalized.
func (s *Server) validateChoiceAndFlagSettings(locale string, payload map[string]any,
	clean map[string]string, providers []string) string {
	for _, key := range sortedKeys(payload) {
		text, ok := pythonStr(payload[key])
		if !ok {
			return s.tr(locale, "{key} must be text or a number", "key", key)
		}
		clean[key] = strings.TrimSpace(text)
	}
	if raw, ok := payload["default_provider"]; ok && pythonTruthy(raw) {
		text, _ := pythonStr(raw)
		if !contains(providers, text) {
			return s.tr(locale, "Invalid default provider")
		}
	}
	if value, ok := clean["captcha_provider"]; ok && value != "none" && !contains(captchaProviders, value) {
		return s.tr(locale, "Invalid CAPTCHA provider")
	}
	for _, key := range booleanSettings {
		if value, ok := clean[key]; ok && value != "0" && value != "1" {
			return s.tr(locale, "{key} must be enabled or disabled", "key", key)
		}
	}
	if _, ok := clean["captcha_hostname"]; ok {
		text, _ := pythonStr(payload["captcha_hostname"])
		hostname := normalizeHostname(text)
		if hostname != "" && (len(hostname) > 253 || !hostnamePattern.MatchString(hostname)) {
			return s.tr(locale, "CAPTCHA hostname must be a hostname without a scheme or port")
		}
		clean["captcha_hostname"] = hostname
	}
	return ""
}

// pythonTruthy is Python truthiness for decoded scalars.
func pythonTruthy(value any) bool {
	text, ok := pythonStr(value)
	if !ok {
		return value != nil
	}
	if _, isString := value.(string); isString {
		return text != ""
	}
	parsed, err := strconv.ParseFloat(text, 64)
	return err != nil || parsed != 0
}

// validateNumericSettings mirrors settings_schema.validate_numeric_settings.
func (s *Server) validateNumericSettings(locale string, clean map[string]string) string {
	for _, key := range []string{"rpm", "width"} {
		text, ok := clean[key]
		if !ok {
			continue
		}
		bounds := numberSettings[key]
		value, err := parsePythonFloat(text)
		if err != nil || !(value >= bounds[0] && value <= bounds[1]) {
			return s.tr(locale, "{key} must be between {minimum} and {maximum}",
				"key", key, "minimum", formatBound(bounds[0]), "maximum", formatBound(bounds[1]))
		}
	}
	for _, key := range integerSettingOrder {
		text, ok := clean[key]
		if !ok {
			continue
		}
		bounds := integerSettings[key]
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || strconv.FormatInt(value, 10) != text || value < bounds[0] || value > bounds[1] {
			return s.tr(locale, "{key} must be a whole number between {minimum} and {maximum}",
				"key", key, "minimum", bounds[0], "maximum", bounds[1])
		}
	}
	return ""
}

func formatBound(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

// parsePythonFloat accepts what Python's float() accepts for stored settings text.
func parsePythonFloat(text string) (float64, error) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(text), "_", "")
	switch strings.ToLower(strings.TrimLeft(cleaned, "+-")) {
	case "inf", "infinity", "nan":
		return strconv.ParseFloat(cleaned, 64)
	}
	if strings.ContainsAny(cleaned, "xXpP") {
		return 0, strconv.ErrSyntax
	}
	return strconv.ParseFloat(cleaned, 64)
}

// captchaSettingsError rejects an update that would leave CAPTCHA unusable,
// evaluated on the merged state because payloads are partial.
func (s *Server) captchaSettingsError(locale string, current panelSettings, clean map[string]string) string {
	combined := map[string]string{}
	for key, value := range current.values {
		combined[key] = value
	}
	for key, value := range clean {
		combined[key] = value
	}
	provider := combined["captcha_provider"]
	if contains(captchaProviders, provider) {
		enabled := false
		for _, item := range captchaActions {
			if combined[item.setting] == "1" {
				enabled = true
			}
		}
		if enabled {
			secretConfigured := combined[provider+"_secret_key"] != "" || current.captchaConfigured[provider]
			if combined[provider+"_site_key"] == "" || !secretConfigured {
				return s.tr(locale, "Configure the selected CAPTCHA site key and secret key before enabling protection")
			}
		}
	}
	if provider != "none" && combined["captcha_hostname"] == "" {
		return s.tr(locale, "Enter the expected CAPTCHA hostname before selecting a CAPTCHA provider")
	}
	return ""
}

// publicAuthConfiguration is the setup-status registration and CAPTCHA block.
func (p panelSettings) publicAuthConfiguration() map[string]any {
	provider := p.get("captcha_provider")
	siteKey := ""
	if contains(captchaProviders, provider) {
		siteKey = p.get(provider + "_site_key")
	}
	actions := []string{}
	if provider != "none" {
		for _, item := range captchaActions {
			if p.get(item.setting) == "1" {
				actions = append(actions, item.action)
			}
		}
	}
	return map[string]any{
		"registration_enabled": p.get("registration_enabled") == "1",
		"captcha": map[string]any{
			"provider": provider, "site_key": siteKey, "protected_actions": actions,
		},
	}
}

func captchaRequired(action string, settings panelSettings) bool {
	provider := settings.get("captcha_provider")
	if !contains(captchaProviders, provider) {
		return false
	}
	for _, item := range captchaActions {
		if item.action == action {
			return settings.get(item.setting) == "1"
		}
	}
	return false
}
