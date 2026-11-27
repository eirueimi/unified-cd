package controller

import (
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
)

// msgServiceAccountToggle is the 400 body for disabling or enabling a service
// account: those are managed in the controller config.
const msgServiceAccountToggle = "service accounts are enabled and disabled in serviceAccounts"

// handleListAccounts lists accounts, optionally filtered by ?kind=service|human.
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != store.AccountKindHuman && kind != store.AccountKindService {
		http.Error(w, "kind must be human or service", http.StatusBadRequest)
		return
	}
	accts, err := s.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	counts, err := s.store.CountPATsByAccount(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]api.Account, 0, len(accts))
	for _, a := range accts {
		if kind != "" && a.Kind != kind {
			continue
		}
		name := a.Name
		if a.Kind == store.AccountKindHuman {
			name = a.Email
			if name == "" {
				name = a.OIDCSub
			}
		}
		out = append(out, api.Account{
			ID: a.ID, Kind: a.Kind, Name: name, Email: a.Email, Disabled: a.DisabledAt != nil,
			CreatedAt: a.CreatedAt, LastSeenAt: a.LastSeenAt, PATCount: counts[a.ID],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDisableAccount(w http.ResponseWriter, r *http.Request) {
	s.setAccountDisabled(w, r, true)
}

func (s *Server) handleEnableAccount(w http.ResponseWriter, r *http.Request) {
	s.setAccountDisabled(w, r, false)
}

func (s *Server) setAccountDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	acct, err := s.store.GetAccount(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if acct == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	if acct.Kind == store.AccountKindService {
		http.Error(w, msgServiceAccountToggle, http.StatusBadRequest)
		return
	}
	if err := s.store.SetAccountDisabled(r.Context(), acct.ID, disabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The id_token path caches the account row per sub; drop it so the change
	// takes effect on this replica at once. Other replicas pick it up when
	// their cached row expires (idTokenAccountTTL).
	s.evictIDTokenAccount(acct.OIDCSub)
	w.WriteHeader(http.StatusNoContent)
}

// evictIDTokenAccount removes the cached id_token account row of sub.
func (s *Server) evictIDTokenAccount(sub string) {
	if sub == "" {
		return
	}
	c := &s.idAccounts
	c.mu.Lock()
	delete(c.entries, sub)
	c.mu.Unlock()
}
