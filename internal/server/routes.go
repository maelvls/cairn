package server

import "net/http"

func (s *Server) routes() {
	mux := s.mux

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Auth
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/config", s.handleAuthConfig)
	mux.HandleFunc("GET /auth/google", s.handleGoogleStart)
	mux.HandleFunc("GET /auth/google/callback", s.handleGoogleCallback)
	mux.HandleFunc("GET /auth/cli", s.handleCLIAuthPage)
	mux.HandleFunc("POST /auth/cli", s.handleCLIAuthConfirm)
	mux.HandleFunc("GET /api/me", s.requireAuth(s.handleMe))
	mux.HandleFunc("GET /api/me/settings", s.requireAuth(s.handleGetSettings))
	mux.HandleFunc("PATCH /api/me/settings", s.requireAuth(s.handleUpdateSettings))
	mux.HandleFunc("POST /api/me/settings/test-notification", s.requireAuth(s.handleTestNotification))

	// OAuth 2.1 for MCP clients (oauth.go)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleAuthServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	if p := s.protectedResourcePath(); p != "/.well-known/oauth-protected-resource" {
		mux.HandleFunc("GET "+p, s.handleProtectedResourceMetadata)
	}
	mux.HandleFunc("POST /oauth/register", s.handleOAuthRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth/authorize", s.handleOAuthApprove)
	mux.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	for _, p := range []string{"/.well-known/oauth-authorization-server", "/oauth/register", "/oauth/token", "/mcp"} {
		mux.HandleFunc("OPTIONS "+p, s.handleOAuthPreflight)
	}

	// User directory (any authenticated user)
	mux.HandleFunc("GET /api/users", s.requireAuth(s.handleUsers))
	mux.HandleFunc("GET /api/users/{id}", s.requireAuth(s.handleUserByID))

	// Admin: users and API keys
	mux.HandleFunc("GET /api/admin/users", s.requireAdmin(s.handleAdminListUsers))
	mux.HandleFunc("POST /api/admin/users", s.requireAdmin(s.handleAdminCreateUser))
	mux.HandleFunc("PATCH /api/admin/users/{id}", s.requireAdmin(s.handleAdminUpdateUser))
	mux.HandleFunc("POST /api/admin/users/{id}/reset-password", s.requireAdmin(s.handleAdminResetPassword))
	mux.HandleFunc("DELETE /api/admin/users/{id}", s.requireAdmin(s.handleAdminDeleteUser))
	mux.HandleFunc("GET /api/admin/keys", s.requireAdmin(s.handleAdminListKeys))
	mux.HandleFunc("POST /api/admin/keys", s.requireAdmin(s.handleAdminCreateKey))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", s.requireAdmin(s.handleAdminRevokeKey))

	// Artifacts: reads follow the public/private flag, writes need auth.
	// The {id} segment accepts an artifact id or a resource reference
	// (resolved by withArtifact/publicAware; ambiguous references are a 409).
	mux.HandleFunc("GET /api/artifacts", s.requireAuth(s.handleListArtifacts))
	mux.HandleFunc("POST /api/artifacts", s.requireAuth(s.handleCreateArtifact))
	mux.HandleFunc("GET /api/artifacts/{id}", s.publicAware(s.handleGetArtifact))
	mux.HandleFunc("PATCH /api/artifacts/{id}", s.requireAuth(s.withArtifact(s.handleUpdateArtifact)))
	mux.HandleFunc("DELETE /api/artifacts/{id}", s.requireAuth(s.withArtifact(s.handleDeleteArtifact)))
	mux.HandleFunc("POST /api/artifacts/{id}/resources", s.requireAuth(s.withArtifact(s.handleAddResource)))
	mux.HandleFunc("DELETE /api/artifacts/{id}/resources/{rid}", s.requireAuth(s.withArtifact(s.handleDeleteResource)))
	mux.HandleFunc("GET /api/artifacts/{id}/versions", s.publicAware(s.handleListVersions))
	mux.HandleFunc("POST /api/artifacts/{id}/versions", s.requireAuth(s.withArtifact(s.handleUploadVersion)))
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{vid}", s.publicAware(s.handleGetVersion))
	mux.HandleFunc("PUT /api/artifacts/{id}/versions/{vid}", s.requireAuth(s.withArtifact(s.handleReplaceVersion)))
	mux.HandleFunc("PATCH /api/artifacts/{id}/versions/{vid}", s.requireAuth(s.withArtifact(s.handleUpdateVersionMeta)))
	mux.HandleFunc("DELETE /api/artifacts/{id}/versions/{vid}", s.requireAuth(s.withArtifact(s.handleDeleteVersion)))
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{vid}/source", s.publicAware(s.handleSourceList))

	// Per-user pins
	mux.HandleFunc("PUT /api/artifacts/{id}/pin", s.requireAuth(s.withArtifact(s.handlePin)))
	mux.HandleFunc("DELETE /api/artifacts/{id}/pin", s.requireAuth(s.withArtifact(s.handlePin)))

	// Comment threads (signed-in users only, public artifacts included)
	mux.HandleFunc("GET /api/artifacts/{id}/comments", s.requireAuth(s.withArtifact(s.handleListComments)))
	mux.HandleFunc("POST /api/artifacts/{id}/comments", s.requireAuth(s.withArtifact(s.handleCreateComment)))
	mux.HandleFunc("PATCH /api/artifacts/{id}/comments/{cid}", s.requireAuth(s.withArtifact(s.handleUpdateComment)))
	mux.HandleFunc("DELETE /api/artifacts/{id}/comments/{cid}", s.requireAuth(s.withArtifact(s.handleDeleteComment)))

	// Shared per-version database (raw SQL proxy)
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{vid}/db/tables", s.publicAware(s.handleDBTables))
	mux.HandleFunc("POST /api/artifacts/{id}/versions/{vid}/db/query", s.publicAware(s.handleDBQuery))
	mux.HandleFunc("POST /api/artifacts/{id}/versions/{vid}/db/batch", s.requireAuth(s.withArtifact(s.handleDBBatch)))
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{vid}/db/download", s.publicAware(s.handleDBDownload))

	// Per-version file storage
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{vid}/files", s.publicAware(s.handleFileList))
	mux.HandleFunc("GET /api/artifacts/{id}/versions/{vid}/files/{path...}", s.publicAware(s.handleFileDownload))
	mux.HandleFunc("PUT /api/artifacts/{id}/versions/{vid}/files/{path...}", s.requireAuth(s.withArtifact(s.handleFileUpload)))
	mux.HandleFunc("DELETE /api/artifacts/{id}/versions/{vid}/files/{path...}", s.requireAuth(s.withArtifact(s.handleFileDelete)))

	// AI runtime (ai.go): signed-in users only, so anonymous visitors of
	// public artifacts never spend the server's Anthropic key.
	mux.HandleFunc("GET /api/me/ai", s.requireAuth(s.handleAIStatus))
	mux.HandleFunc("POST /api/artifacts/{id}/ai", s.requireAuth(s.withArtifact(s.handleAI)))

	// MCP (Streamable HTTP, stateless: no SSE stream to GET)
	mux.HandleFunc("POST /mcp", s.requireMCPAuth(s.handleMCP))
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "this MCP server does not stream; POST JSON-RPC messages")
	})

	// Pages
	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("GET /admin", s.handleAdminPage)
	mux.HandleFunc("GET /logout", s.handleLogoutPage)
	mux.HandleFunc("GET /cairn.js", s.serveCairnJS)
	mux.HandleFunc("GET /_ui/{file}", s.serveUIAsset)
	mux.HandleFunc("GET /artifacts/{id}", s.handleArtifactRedirect)
	mux.HandleFunc("GET /artifacts/{id}/{vid}", s.handleVersionNoSlash)
	mux.HandleFunc("GET /artifacts/{id}/{vid}/{path...}", s.handleVersionPage)
	mux.HandleFunc("GET /shared/{id}", s.handleShared)
	mux.HandleFunc("GET /shared/{id}/{vid}", s.handleShared)
}
