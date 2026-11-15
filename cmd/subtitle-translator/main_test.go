package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"healthy", `{"status":"ok"}`, 200, false},
		{"failure", `{"status":"ok"}`, 503, true},
		{"invalid status", `{"status":"failed"}`, 200, true},
		{"invalid JSON", "not JSON", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			err := checkHealth(strings.TrimPrefix(server.URL, "http://"), "")
			if (err != nil) != tc.wantError {
				t.Errorf("checkHealth() = %v, want error %v", err, tc.wantError)
			}
		})
	}
}
