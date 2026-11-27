package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/store"
	"golang.org/x/time/rate"
)

const (
	// InitialTokenFileName is the file the generated initial admin token is
	// written to (mode 0600).
	InitialTokenFileName = "initial-admin-token"

	initialTokenPrefix = "uci_"

	// bootstrapPATName is the default name of the PAT handed out by the
	// bootstrap exchange; it is also the audit actor id of that exchange.
	bootstrapPATName = "bootstrap"

	// bootstrapPATLifetime is the lifetime of the PAT handed out by the
	// bootstrap exchange (the built-in admin account's maximum).
	bootstrapPATLifetime = adminTokenMaxLifetime

	bootstrapRateLimit = 5 // requests per minute per controller process

	errBootstrapInvalid     = "invalid or already used initial token"
	errBootstrapRateLimited = "too many bootstrap attempts"
)

// GenerateInitialToken returns a new initial admin token: "uci_" followed by
// 40 hex characters.
func GenerateInitialToken() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return initialTokenPrefix + hex.EncodeToString(buf), nil
}

// RemoveInitialTokenFiles deletes the initial-admin-token file from each of
// dirs and returns the paths it removed. A file that does not exist or cannot
// be deleted is skipped.
func RemoveInitialTokenFiles(dirs []string) []string {
	var removed []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := filepath.Join(d, InitialTokenFileName)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Remove(p); err != nil {
			slog.Warn("could not remove the used initial admin token file", "path", p, "error", err)
			continue
		}
		removed = append(removed, p)
	}
	return removed
}

func newBootstrapLimiter() *rate.Limiter {
	return rate.NewLimiter(rate.Every(time.Minute/bootstrapRateLimit), bootstrapRateLimit)
}

// handleBootstrap exchanges the one-time initial token for a PAT of the
// built-in admin account. The route is unauthenticated; the initial token is
// the credential. Every failure to match answers the same 401.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !s.bootstrapLimiter.Allow() {
		http.Error(w, errBootstrapRateLimited, http.StatusTooManyRequests)
		return
	}
	var req struct {
		InitialToken string `json:"initialToken"`
		Name         string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.InitialToken == "" {
		http.Error(w, errBootstrapInvalid, http.StatusUnauthorized)
		return
	}
	name := req.Name
	if name == "" {
		name = bootstrapPATName
	}
	if strings.HasPrefix(name, "env:") || strings.HasPrefix(name, "system:") {
		http.Error(w, fmt.Sprintf("token name %q is reserved", name), http.StatusBadRequest)
		return
	}

	ok, err := s.store.ConsumeInitialToken(r.Context(), HashToken(req.InitialToken))
	if err != nil {
		slog.Error("bootstrap: consume initial token", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !ok {
		slog.Warn("bootstrap: invalid or already used initial token", "remote", normalizedRemoteIP(r.RemoteAddr))
		http.Error(w, errBootstrapInvalid, http.StatusUnauthorized)
		return
	}

	accts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		slog.Error("bootstrap: list accounts", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var adminID string
	for _, a := range accts {
		if a.Kind == store.AccountKindService && a.Name == authz.BuiltinAdminAccount {
			adminID = a.ID
			break
		}
	}
	if adminID == "" {
		// Cannot happen: the admin row is created by migration. The initial
		// token is already consumed; the recovery command remains.
		slog.Error("bootstrap: the built-in admin account is missing")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	token, err := generatePAT()
	if err != nil {
		slog.Error("bootstrap: token generation", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(bootstrapPATLifetime)
	pat, err := s.store.CreatePAT(r.Context(), store.NewPAT{
		Name: name, TokenHash: HashToken(token), AccountID: adminID, ExpiresAt: &expires, CreatedBy: bootstrapPATName,
	})
	if err != nil {
		// The initial token is consumed; the operator can use the recovery command.
		slog.Error("bootstrap: create PAT (the initial token is already consumed; use 'unified-controller admin recover')", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.store.InsertAuditLog(r.Context(), store.AuditEntry{
		Actor: bootstrapPATName, ActorID: bootstrapPATName, Method: http.MethodPost, Path: "/api/v1/bootstrap",
		Action: "bootstrap.exchange", Resource: name, Status: http.StatusCreated,
		Details: map[string]any{"patId": pat.ID},
	}); err != nil {
		slog.Warn("bootstrap: audit log", "error", err)
	}
	for _, p := range RemoveInitialTokenFiles(s.cfg.InitialTokenDirs) {
		slog.Info("removed the used initial admin token file", "path", p)
	}
	writeJSON(w, http.StatusCreated, api.CreatePATResponse{
		ID: pat.ID, Name: pat.Name, Role: pat.Role, Token: token,
		Permissions: pat.Permissions, EnrollLabels: pat.EnrollLabels,
	})
}
