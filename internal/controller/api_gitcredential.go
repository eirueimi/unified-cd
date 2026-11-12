package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// errNameCollision marks a GitCredential/StoredSecret name clash in the shared
// secrets namespace. The API maps it to 409 Conflict.
var errNameCollision = errors.New("name collision")

type nameCollisionError struct{ msg string }

func (e nameCollisionError) Error() string        { return e.msg }
func (e nameCollisionError) Is(target error) bool { return target == errNameCollision }

// gitCredentialFromDSL validates gc and converts it to the stored form. Job
// access folders are normalized with authz.NormalizeFolder ("*" is kept as-is).
// It is shared by the API handler and GitOps apply.
func gitCredentialFromDSL(gc *dsl.GitCredential) (store.GitCredential, error) {
	if err := gc.Validate(); err != nil {
		return store.GitCredential{}, err
	}
	out := store.GitCredential{
		Name: gc.Metadata.Name, Host: gc.Spec.Host, CredType: gc.Spec.Type,
		SecretRef: gc.Spec.SecretRef, KnownHosts: gc.Spec.KnownHosts,
		ControllerUse: gc.Spec.ControllerUseOrDefault(),
	}
	if a := gc.Spec.GitHubApp; a != nil {
		out.GitHubApp = &api.GitHubAppConfig{AppID: a.AppID, InstallationID: a.InstallationID, APIURL: a.APIURL}
	}
	if ja := gc.Spec.JobAccess; ja != nil {
		folders := make([]string, 0, len(ja.Folders))
		for i, f := range ja.Folders {
			if f != "*" {
				n, err := authz.NormalizeFolder(f)
				if err != nil {
					return store.GitCredential{}, fmt.Errorf("spec.jobAccess.folders[%d] %q: %w", i, f, err)
				}
				f = n
			}
			folders = append(folders, f)
		}
		perms := make(map[string]string, len(ja.Permissions))
		for k, v := range ja.Permissions {
			perms[k] = v
		}
		out.JobAccess = &api.GitJobAccess{
			Folders:      folders,
			Repositories: append([]string(nil), ja.Repositories...),
			Permissions:  perms,
		}
	}
	return out, nil
}

// checkSecretNamespaceCollision rejects a GitCredential with jobAccess whose
// name is already taken by a StoredSecret (the two share the secrets
// namespace). A failure other than a collision is returned unwrapped.
func checkSecretNamespaceCollision(ctx context.Context, st store.Store, gc store.GitCredential) error {
	if gc.JobAccess == nil {
		return nil
	}
	_, err := st.GetSecret(ctx, gc.Name)
	if err == nil {
		return nameCollisionError{fmt.Sprintf("name %q is already used by a StoredSecret; a GitCredential with jobAccess shares the secrets namespace", gc.Name)}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

func (s *Server) handleUpsertGitCredential(w http.ResponseWriter, r *http.Request) {
	var req api.UpsertGitCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := dsl.ValidateName(req.Name); err != nil {
		http.Error(w, "name "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Host == "" || req.CredType == "" || req.SecretRef == "" {
		http.Error(w, "host, credType, secretRef are required", http.StatusBadRequest)
		return
	}
	d := &dsl.GitCredential{
		APIVersion: dsl.SupportedAPIVersion, Kind: "GitCredential",
		Metadata: dsl.Metadata{Name: req.Name},
		Spec: dsl.GitCredentialSpec{
			Host: req.Host, Type: req.CredType, SecretRef: req.SecretRef,
			KnownHosts: req.KnownHosts, ControllerUse: req.ControllerUse,
		},
	}
	if a := req.GitHubApp; a != nil {
		d.Spec.GitHubApp = &dsl.GitHubAppSpec{AppID: a.AppID, InstallationID: a.InstallationID, APIURL: a.APIURL}
	}
	if ja := req.JobAccess; ja != nil {
		d.Spec.JobAccess = &dsl.GitJobAccessSpec{Folders: ja.Folders, Repositories: ja.Repositories, Permissions: ja.Permissions}
	}
	gc, err := gitCredentialFromDSL(d)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := checkSecretNamespaceCollision(r.Context(), s.store, gc); err != nil {
		if errors.Is(err, errNameCollision) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.guardManagedResource(r.Context(), "GitCredential", req.Name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.UpsertGitCredential(r.Context(), gc); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListGitCredentials(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListGitCredentials(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	result := make([]api.GitCredentialMeta, 0, len(list))
	for _, gc := range list {
		result = append(result, api.GitCredentialMeta{
			ID: gc.ID, Name: gc.Name, Host: gc.Host,
			CredType: gc.CredType, SecretRef: gc.SecretRef, KnownHosts: gc.KnownHosts, ControllerUse: gc.ControllerUse,
			GitHubApp: gc.GitHubApp, JobAccess: gc.JobAccess, CreatedAt: gc.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDeleteGitCredential(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.guardManagedResource(r.Context(), "GitCredential", name); err != nil {
		writeGuardError(w, err)
		return
	}
	if err := s.store.DeleteGitCredential(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
