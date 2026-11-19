package controller

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/go-chi/chi/v5"
)

// auditActionObjectsStream is the action of the audit rows the streaming
// object routes write for uploads.
const auditActionObjectsStream = "objects.stream"

// cacheKeyHeader carries the original key of the cache entry a restore
// matched, percent-encoded with url.PathEscape.
const cacheKeyHeader = "X-Unified-Cache-Key"

// The controller-mode routes move archive bytes through the controller, for
// stores that cannot presign (the local filesystem store) and for agents
// configured with objectTransfer: controller. They authenticate and scope
// exactly like the presign endpoint (authenticateObjectRequester) and derive
// every object key on the server: an artifact as artifact.ArtifactKey, a cache
// entry as cache.DataKey(<run record's job>, key). A cache upload writes its
// .meta here, with the size it counted and the TTL rule of the commit
// operation.

// streamStore returns the object store, or writes the 503 and returns false.
func (s *Server) streamStore(w http.ResponseWriter) (objectstore.ObjectStore, bool) {
	if s.objStore == nil {
		writeObjectError(w, http.StatusServiceUnavailable, api.ObjectErrNoObjectStore, "object store not configured")
		return nil, false
	}
	return s.objStore, true
}

func writeObjectErr(w http.ResponseWriter, oerr *objectErr) {
	writeObjectError(w, oerr.status, oerr.code, oerr.msg)
}

// handleStreamArtifactPut handles PUT /api/v1/runs/{runID}/objects/artifacts/{name}.
// An artifact can only be written to the requester's own run.
func (s *Server) handleStreamArtifactPut(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	rq, ok := s.authenticateObjectRequester(w, r, runID)
	if !ok {
		return
	}
	store, ok := s.streamStore(w)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	key, err := artifact.ArtifactKey(runID, name)
	if err != nil {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "invalid artifact name")
		return
	}
	status := http.StatusNoContent
	if err := store.Put(r.Context(), key, r.Body, contentLengthOrUnknown(r)); err != nil {
		slog.Warn("stream: artifact put failed", "key", key, "error", err)
		status = http.StatusInternalServerError
	}
	s.auditStream(r, rq, "artifact:"+runID+"/"+name, map[string]any{"runId": runID, "kind": api.ObjectKindArtifact, "op": api.ObjectOpPut}, status)
	if status != http.StatusNoContent {
		writeObjectError(w, status, api.ObjectErrInternal, "put failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStreamArtifactGet handles GET /api/v1/runs/{runID}/objects/artifacts/{name}
// with an optional ?sourceRunId=<id> to read another run's artifact.
func (s *Server) handleStreamArtifactGet(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	if _, ok := s.authenticateObjectRequester(w, r, runID); !ok {
		return
	}
	store, ok := s.streamStore(w)
	if !ok {
		return
	}
	key, oerr := s.artifactReadKey(r.Context(), runID, r.URL.Query().Get("sourceRunId"), chi.URLParam(r, "name"))
	if oerr != nil {
		writeObjectErr(w, oerr)
		return
	}
	rc, err := store.Get(r.Context(), key)
	if errors.Is(err, objectstore.ErrNotFound) {
		writeObjectError(w, http.StatusNotFound, api.ObjectErrNotFound, "object not found")
		return
	}
	if err != nil {
		writeObjectErr(w, internalObject("get", key, err))
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

// handleStreamCachePut handles PUT /api/v1/runs/{runID}/objects/caches?key=<k>&ttlDays=<n>.
// The archive is stored and .meta committed here, so the response is 204 only
// once the entry is visible to restores.
func (s *Server) handleStreamCachePut(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	rq, ok := s.authenticateObjectRequester(w, r, runID)
	if !ok {
		return
	}
	store, ok := s.streamStore(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	key := q.Get("key")
	if key == "" {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "key is required")
		return
	}
	requested := 0
	if v := q.Get("ttlDays"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "ttlDays must be an integer")
			return
		}
		requested = n
	}
	spec, err := s.store.GetRunSpec(r.Context(), runID)
	if err != nil {
		writeObjectErr(w, internalObject("load run spec", cache.DataKey(rq.JobName, key), err))
		return
	}
	ttl, err := cacheTTLDays(spec, requested)
	if err != nil {
		writeObjectErr(w, internalObject("parse run spec", cache.DataKey(rq.JobName, key), err))
		return
	}
	status := http.StatusNoContent
	remote := &cache.StoreRemote{Store: store, JobName: rq.JobName}
	if err := remote.Save(r.Context(), key, ttl, r.Body); err != nil {
		slog.Warn("stream: cache save failed", "key", cache.DataKey(rq.JobName, key), "error", err)
		status = http.StatusInternalServerError
	}
	s.auditStream(r, rq, "cache:"+rq.JobName+"/"+key, map[string]any{"runId": runID, "kind": api.ObjectKindCache, "op": api.ObjectOpPut}, status)
	if status != http.StatusNoContent {
		writeObjectError(w, status, api.ObjectErrInternal, "cache save failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleStreamCacheGet handles GET /api/v1/runs/{runID}/objects/caches?key=<k>&restoreKey=<p>...
func (s *Server) handleStreamCacheGet(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	rq, ok := s.authenticateObjectRequester(w, r, runID)
	if !ok {
		return
	}
	store, ok := s.streamStore(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	key := q.Get("key")
	if key == "" {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "key is required")
		return
	}
	remote := &cache.StoreRemote{Store: store, JobName: rq.JobName}
	rc, matched, err := remote.Open(r.Context(), key, q["restoreKey"])
	if errors.Is(err, cache.ErrCacheMiss) {
		writeObjectError(w, http.StatusNotFound, api.ObjectErrCacheMiss, "cache miss")
		return
	}
	if err != nil {
		writeObjectErr(w, internalObject("cache open", cache.JobPrefix(rq.JobName), err))
		return
	}
	defer rc.Close()
	w.Header().Set(cacheKeyHeader, url.PathEscape(matched))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func contentLengthOrUnknown(r *http.Request) int64 {
	if r.ContentLength < 0 {
		return -1
	}
	return r.ContentLength
}

// auditStream writes an objects.stream row for an upload; details never carry
// object bytes or credentials.
func (s *Server) auditStream(r *http.Request, rq objectRequester, resource string, details map[string]any, status int) {
	if rq.PodUID != "" {
		details["podUid"] = rq.PodUID
	}
	s.auditAgentEvent(r, rq.AgentID, auditActionObjectsStream, resource, status, details)
}
