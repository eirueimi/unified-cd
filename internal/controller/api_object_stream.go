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
// matched. The value is percent-encoded with url.PathEscape (so any key is a
// valid header value) and decoded by the client; ordinary keys are unchanged.
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
	rq, ok := s.authenticateObjectRequesterAs(w, r, runID, auditActionObjectsStream)
	if !ok {
		return
	}
	store, ok := s.streamStore(w)
	if !ok {
		return
	}
	name, err := artifactNameParam(r)
	if err != nil {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "invalid artifact name")
		return
	}
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
	if _, ok := s.authenticateObjectRequesterAs(w, r, runID, auditActionObjectsStream); !ok {
		return
	}
	store, ok := s.streamStore(w)
	if !ok {
		return
	}
	name, err := artifactNameParam(r)
	if err != nil {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "invalid artifact name")
		return
	}
	key, oerr := s.artifactReadKey(r.Context(), runID, r.URL.Query().Get("sourceRunId"), name)
	if oerr != nil {
		writeObjectErr(w, oerr)
		return
	}
	size := int64(-1)
	if st, ok := store.(objectstore.Statter); ok {
		info, err := st.Stat(r.Context(), key)
		if errors.Is(err, objectstore.ErrNotFound) {
			writeObjectError(w, http.StatusNotFound, api.ObjectErrNotFound, "object not found")
			return
		}
		if err != nil {
			writeObjectErr(w, internalObject("stat", key, err))
			return
		}
		size = info.Size
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
	streamBody(w, rc, size, key, "")
}

// handleStreamCachePut handles PUT /api/v1/runs/{runID}/objects/caches?key=<k>&ttlDays=<n>.
// The archive is stored and .meta committed here, so the response is 204 only
// once the entry is visible to restores.
func (s *Server) handleStreamCachePut(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	rq, ok := s.authenticateObjectRequesterAs(w, r, runID, auditActionObjectsStream)
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
	rq, ok := s.authenticateObjectRequesterAs(w, r, runID, auditActionObjectsStream)
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
	rc, matched, size, err := s.openCache(r, store, rq.JobName, key, q["restoreKey"])
	if errors.Is(err, cache.ErrCacheMiss) {
		writeObjectError(w, http.StatusNotFound, api.ObjectErrCacheMiss, "cache miss")
		return
	}
	if err != nil {
		writeObjectErr(w, internalObject("cache open", cache.JobPrefix(rq.JobName), err))
		return
	}
	defer rc.Close()
	// The matched key is percent-encoded (url.PathEscape) so any key is a
	// valid header value; the client decodes it. Ordinary keys are unchanged.
	streamBody(w, rc, size, cache.JobPrefix(rq.JobName), url.PathEscape(matched))
}

// openCache opens the archive for key (then restoreKeys). When the store can
// Stat, the lookup is cache.LookupExisting, as for presign: a .meta whose
// archive is gone falls through to the next candidate, and the size is known.
func (s *Server) openCache(r *http.Request, store objectstore.ObjectStore, job, key string, restoreKeys []string) (io.ReadCloser, string, int64, error) {
	ctx := r.Context()
	st, ok := store.(objectstore.Statter)
	if !ok {
		rc, matched, err := (&cache.StoreRemote{Store: store, JobName: job}).Open(ctx, key, restoreKeys)
		return rc, matched, -1, err
	}
	dataKey, matched, info, err := cache.LookupExisting(ctx, store, st, job, key, restoreKeys)
	if err != nil {
		return nil, "", 0, err
	}
	rc, err := store.Get(ctx, dataKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		return nil, "", 0, cache.ErrCacheMiss
	}
	if err != nil {
		return nil, "", 0, err
	}
	return rc, matched, info.Size, nil
}

// artifactNameParam is the {name} URL parameter, percent-decoded: chi returns
// the still-escaped segment when the path has escapes (a name with "," or ";"
// is sent escaped), while direct mode stores the plain name.
func artifactNameParam(r *http.Request) (string, error) {
	return url.PathUnescape(chi.URLParam(r, "name"))
}

// streamBody sends rc as a 200 response. A known size is declared as
// Content-Length so the client can detect truncation. If the copy fails after
// the headers went out, the connection is aborted (http.ErrAbortHandler)
// instead of ending the chunked body cleanly, which a client would take for a
// complete download.
func streamBody(w http.ResponseWriter, rc io.Reader, size int64, logKey, matchedKeyEscaped string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	if matchedKeyEscaped != "" {
		w.Header().Set(cacheKeyHeader, matchedKeyEscaped)
	}
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		slog.Warn("stream: download failed part way", "key", logKey, "error", err)
		panic(http.ErrAbortHandler)
	}
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
	s.auditPresignAction(r, rq, auditActionObjectsStream, resource, details, status)
}
