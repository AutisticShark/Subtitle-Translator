package app

import (
	"net/http"
	"sort"
	"strings"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request, _ *authResult) {
	settings, err := s.readSettings(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings.publicJSON(s, localeOf(r)))
}

func allSettingKeys(defaults map[string]string) map[string]bool {
	keys := map[string]bool{}
	for key := range defaults {
		keys[key] = true
	}
	for _, key := range secretKeys {
		keys[key] = true
	}
	return keys
}

// handleSaveSettings validates a partial update, then writes it under the
// shared settings-row lock (rule 21); secrets are stored encrypted and are
// never returned.
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request, _ *authResult) {
	payload, ok := s.jsonPayload(w, r)
	if !ok {
		return
	}
	locale := localeOf(r)
	known := allSettingKeys(s.defaults)
	var unknown []string
	for key := range payload {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		s.jsonError(w, r, http.StatusBadRequest, "Unknown settings: {settings}", "settings", strings.Join(unknown, ", "))
		return
	}
	clean := map[string]string{}
	if message := s.validateChoiceAndFlagSettings(locale, payload, clean, s.availableProviders()); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	for _, key := range secretKeys {
		if value, ok := clean[key]; ok && value != "" && !s.stableKeys {
			s.jsonError(w, r, http.StatusServiceUnavailable, "Configure JWT_SECRET_KEY before saving secrets")
			return
		}
	}
	if message := s.validateNumericSettings(locale, clean); message != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": message})
		return
	}
	ctx := r.Context()
	err := s.db.tx(ctx, func(c conn) error {
		if err := lockSettings(ctx, c); err != nil {
			return err
		}
		current, stored, err := s.storedSettings(ctx, c)
		if err != nil {
			return err
		}
		if message := s.captchaSettingsError(locale, current, clean); message != "" {
			return &txResponse{http.StatusBadRequest, message}
		}
		changedWindow := false
		for _, key := range windowLimitKeys {
			value, present := clean[key]
			if !present {
				continue
			}
			previous, ok := stored[key]
			if !ok {
				previous = s.defaults[key]
			}
			if value != previous {
				changedWindow = true
			}
		}
		timestamp := isoSeconds(s.now())
		for _, key := range sortedKeys(clean) {
			value := clean[key]
			if isSecretKey(key) {
				if value == "" {
					continue
				}
				value = s.encryptSecret(value)
			}
			if _, exists := stored[key]; exists {
				_, err = c.exec(ctx, "UPDATE settings SET value = ?, updated_at = ? WHERE name = ?", value, timestamp, key)
			} else {
				_, err = c.exec(ctx, "INSERT INTO settings (name, value, updated_at) VALUES (?, ?, ?)", key, value, timestamp)
			}
			if err != nil {
				return err
			}
		}
		if changedWindow {
			// Calendar usage survives all settings edits, including disabling limits.
			if _, err := c.exec(ctx, "DELETE FROM rate_limit_buckets WHERE scope NOT LIKE ? AND scope NOT LIKE ? "+
				"AND scope NOT LIKE ?", "%:daily", "%:weekly", "%:monthly"); err != nil {
				return err
			}
		}
		return bumpCacheRevision(ctx, c, "settings")
	})
	if s.writeTxError(w, r, err) {
		return
	}
	s.handleGetSettings(w, r, nil)
}

// handleDeleteKey removes a saved provider API key or CAPTCHA secret.
func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request, _ *authResult) {
	provider := r.PathValue("provider")
	captchaProvider, isCaptcha := strings.CutPrefix(provider, "captcha-")
	key := provider + "_api_key"
	if isCaptcha {
		key = captchaProvider + "_secret_key"
	}
	if !isSecretKey(key) {
		s.jsonError(w, r, http.StatusNotFound, "Unknown provider")
		return
	}
	ctx := r.Context()
	err := s.db.tx(ctx, func(c conn) error {
		if err := lockSettings(ctx, c); err != nil {
			return err
		}
		current, _, err := s.storedSettings(ctx, c)
		if err != nil {
			return err
		}
		if contains(captchaProviders, captchaProvider) && current.get("captcha_provider") == captchaProvider {
			for _, item := range captchaActions {
				if current.get(item.setting) == "1" {
					return &txResponse{http.StatusConflict, "Disable CAPTCHA before removing its active secret key"}
				}
			}
		}
		if _, err := c.exec(ctx, "DELETE FROM settings WHERE name = ?", key); err != nil {
			return err
		}
		return bumpCacheRevision(ctx, c, "settings")
	})
	if s.writeTxError(w, r, err) {
		return
	}
	s.handleGetSettings(w, r, nil)
}
