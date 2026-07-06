package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// errManagedResource marks a write rejected because the target resource is
// managed by an AppSource. Handlers translate it to 409 Conflict.
type errManagedResource struct {
	Kind      string
	Name      string
	AppSource string
	RepoURL   string
}

func (e errManagedResource) Error() string {
	msg := fmt.Sprintf("resource %s %q is managed by AppSource %q; update it in Git", e.Kind, e.Name, e.AppSource)
	if e.RepoURL != "" {
		msg += " (" + e.RepoURL + ")"
	}
	return msg + ", or set syncPolicy.allowManualOverride: true on the AppSource"
}

// guardManagedResource rejects direct writes (apply/delete) to resources managed
// by an AppSource, so Git stays the source of truth for synced resources.
// Fail-close: store errors reject the write (500), and an unparseable manager
// spec rejects it too (409, since the management fact itself is known).
// Exceptions: the managing AppSource's syncPolicy.allowManualOverride, and an
// AppSource that manages itself (app-of-apps root must stay repairable).
func (s *Server) guardManagedResource(ctx context.Context, kind, name string) error {
	src, err := s.store.FindManagingAppSource(ctx, kind, name)
	if err != nil {
		return fmt.Errorf("check managed resource: %w", err)
	}
	if src == nil {
		return nil
	}
	if kind == "AppSource" && src.Name == name {
		return nil
	}
	var spec dsl.AppSourceSpec
	if err := json.Unmarshal(src.Spec, &spec); err != nil {
		return errManagedResource{Kind: kind, Name: name, AppSource: src.Name}
	}
	if spec.SyncPolicy.AllowManualOverride {
		return nil
	}
	return errManagedResource{Kind: kind, Name: name, AppSource: src.Name, RepoURL: spec.RepoURL}
}

// writeGuardError maps a guardManagedResource error onto the HTTP response:
// 409 for managed-resource rejections, 500 for infrastructure failures.
func writeGuardError(w http.ResponseWriter, err error) {
	var m errManagedResource
	if errors.As(err, &m) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
