package app

import "net/http"

// routes registers the HTTP API of the Python application, path for path.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/i18n", s.handleI18n)

	mux.HandleFunc("GET /api/auth/setup-status", s.handleSetupStatus)
	mux.HandleFunc("POST /api/auth/setup", s.handleSetup)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/logout", s.noRefresh(s.requireAuth(s.handleLogout)))
	mux.HandleFunc("GET /api/auth/me", s.requireAuth(s.handleMe))
	mux.HandleFunc("PATCH /api/auth/me", s.requireAuth(s.handleUpdateMe))

	mux.HandleFunc("GET /api/auth/mfa", s.noRefresh(s.requireAuth(s.handleMFAStatus)))
	mux.HandleFunc("POST /api/auth/mfa/setup", s.noRefresh(s.requireAuth(s.handleMFASetup)))
	mux.HandleFunc("POST /api/auth/mfa/confirm", s.noRefresh(s.requireAuth(s.handleMFAConfirm)))
	mux.HandleFunc("POST /api/auth/mfa/email", s.noRefresh(s.requireAuth(s.handleMFAManagementEmail)))
	mux.HandleFunc("POST /api/auth/mfa/disable", s.noRefresh(s.requireAuth(s.handleMFADisable)))
	mux.HandleFunc("POST /api/auth/mfa/recovery", s.noRefresh(s.requireAuth(s.handleMFARecovery)))
	mux.HandleFunc("POST /api/auth/mfa/verify", s.noRefresh(s.handleMFAVerify))
	mux.HandleFunc("POST /api/auth/mfa/resend", s.noRefresh(s.handleMFAResend))

	mux.HandleFunc("GET /api/users", s.requireAdmin(s.handleListUsers))
	mux.HandleFunc("POST /api/users", s.requireAdmin(s.handleCreateUser))
	mux.HandleFunc("PATCH /api/users/{user_id}", s.requireAdmin(s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/users/{user_id}", s.requireAdmin(s.handleDeleteUser))

	mux.HandleFunc("GET /api/settings", s.requireAuth(s.handleGetSettings))
	mux.HandleFunc("PUT /api/settings", s.requireAdmin(s.handleSaveSettings))
	mux.HandleFunc("DELETE /api/settings/keys/{provider}", s.requireAdmin(s.handleDeleteKey))

	mux.HandleFunc("POST /api/jobs", s.requireAuth(s.handleCreateJobs))
	mux.HandleFunc("GET /api/jobs", s.requireAuth(s.handleListJobs))
	mux.HandleFunc("GET /api/jobs/{job_id}", s.requireAuth(s.handleGetJob))
	mux.HandleFunc("POST /api/jobs/{job_id}/cancel", s.requireAuth(s.handleCancelJob))
	mux.HandleFunc("DELETE /api/jobs/{job_id}", s.requireAuth(s.handleDeleteJob))
	mux.HandleFunc("GET /api/jobs/{job_id}/download", s.requireAuth(s.handleDownloadAll))
	mux.HandleFunc("GET /api/jobs/{job_id}/download/{name...}", s.requireAuth(s.handleDownloadOutput))

	mux.HandleFunc("/", s.handleStatic)
	return s.middleware(mux)
}

// noRefresh marks endpoints whose responses establish or end a session
// themselves (logout, MFA): refreshing the request's previous cookie would
// overwrite or resurrect that decision.
func (s *Server) noRefresh(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		state(r).noRefresh = true
		next(w, r)
	}
}
