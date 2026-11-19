package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/go-chi/chi/v5"
)

// auditActionObjectsPresign is the action of the audit rows the presign
// endpoint writes for write operations and for denials.
const auditActionObjectsPresign = "objects.presign"

// maxPresignBodyBytes bounds a presign request body. The largest legitimate
// body is a multipart-complete with 10,000 parts (well under 1 MiB).
const maxPresignBodyBytes = 1 << 20

// Cache TTL bounds, mirroring the DSL (dsl.CacheStep.TTLDays: default 30,
// max 365).
const (
	defaultCacheTTLDays = 30
	maxCacheTTLDays     = 365
)

// objectErr is a presign failure: the HTTP status, the ObjectError code and a
// message safe to return (never a URL or credential).
type objectErr struct {
	status int
	code   string
	msg    string
}

func invalidObject(format string, args ...any) *objectErr {
	return &objectErr{status: http.StatusBadRequest, code: api.ObjectErrInvalid, msg: fmt.Sprintf(format, args...)}
}

func internalObject(op, key string, err error) *objectErr {
	// Log the key and the store error only; a URL is never part of either.
	slog.Warn("presign: store operation failed", "op", op, "key", key, "error", err)
	return &objectErr{status: http.StatusInternalServerError, code: api.ObjectErrInternal, msg: op + " failed"}
}

// presignWriteOps are the operations that change the store (or start doing
// so) and are therefore audited.
var presignWriteOps = map[string]bool{
	api.ObjectOpPut:               true,
	api.ObjectOpMultipartStart:    true,
	api.ObjectOpMultipartComplete: true,
	api.ObjectOpMultipartAbort:    true,
	api.ObjectOpCommit:            true,
}

var presignOps = map[string]bool{
	api.ObjectOpGet: true, api.ObjectOpHead: true, api.ObjectOpPut: true,
	api.ObjectOpMultipartStart: true, api.ObjectOpMultipartParts: true,
	api.ObjectOpMultipartComplete: true, api.ObjectOpMultipartAbort: true,
	api.ObjectOpCommit: true,
}

// handlePresignObject handles POST /api/v1/runs/{runID}/objects/presign.
//
// The requester is authenticated and bound to the run first
// (authenticateObjectRequester). Every object key is then derived here: an
// artifact as artifact.ArtifactKey(run, name), a cache entry as
// cache.DataKey(<run record's job>, key). The client never sends a key, and
// no URL is ever signed for a cache .meta object: the controller writes .meta
// itself in the commit operation, with the size it reads from the store.
//
// URLs in the response are bearer credentials: they are never logged or
// audited.
func (s *Server) handlePresignObject(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	requester, ok := s.authenticateObjectRequester(w, r, runID)
	if !ok {
		return
	}
	if s.objStore == nil {
		writeObjectError(w, http.StatusServiceUnavailable, api.ObjectErrNoObjectStore, "object store not configured")
		return
	}
	var req api.PresignRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPresignBodyBytes)).Decode(&req); err != nil {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "invalid JSON body")
		return
	}
	if req.Kind != api.ObjectKindArtifact && req.Kind != api.ObjectKindCache {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, `kind must be "artifact" or "cache"`)
		return
	}
	if !presignOps[req.Op] {
		writeObjectError(w, http.StatusBadRequest, api.ObjectErrInvalid, "unknown op "+fmt.Sprintf("%q", req.Op))
		return
	}
	if s.presigner == nil {
		// The store cannot presign (the local filesystem store): the client
		// streams through the controller instead.
		writeJSON(w, http.StatusOK, api.PresignResponse{Mode: api.ObjectModeController})
		return
	}

	var (
		resp     api.PresignResponse
		oerr     *objectErr
		resource string
	)
	if req.Kind == api.ObjectKindArtifact {
		resource = "artifact:" + runID + "/" + req.Name
		resp, oerr = s.presignArtifact(r.Context(), runID, req)
	} else {
		resource = "cache:" + requester.JobName + "/" + req.Key
		resp, oerr = s.presignCache(r.Context(), requester, req)
	}
	status := http.StatusOK
	if oerr != nil {
		status = oerr.status
	}
	if presignWriteOps[req.Op] {
		s.auditPresign(r, requester, resource, map[string]any{"runId": runID, "kind": req.Kind, "op": req.Op}, status)
	}
	if oerr != nil {
		writeObjectError(w, oerr.status, oerr.code, oerr.msg)
		return
	}
	resp.Mode = api.ObjectModeDirect
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) presignArtifact(ctx context.Context, runID string, req api.PresignRequest) (api.PresignResponse, *objectErr) {
	if req.Key != "" || len(req.RestoreKeys) > 0 || req.TTLDays != 0 {
		return api.PresignResponse{}, invalidObject("key, restoreKeys and ttlDays apply to caches only")
	}
	if req.Op == api.ObjectOpCommit {
		return api.PresignResponse{}, invalidObject("commit applies to caches only")
	}
	if req.SourceRunID != "" && req.SourceRunID != runID && req.Op != api.ObjectOpGet && req.Op != api.ObjectOpHead {
		return api.PresignResponse{}, invalidObject("sourceRunId is only allowed for get and head")
	}
	key, oerr := s.artifactReadKey(ctx, runID, req.SourceRunID, req.Name)
	if oerr != nil {
		return api.PresignResponse{}, oerr
	}
	return s.presignKey(ctx, key, req, req.Name+".tar.gz", api.ObjectErrNotFound)
}

// artifactReadKey derives the key of artifact name in the requester's run, or
// in sourceRunID when that names another run. Reading another run's artifact
// keeps the existing download route's rule for machine principals: any
// authenticated requester may read an existing run's artifacts
// (downloadArtifact.runId, call: children).
func (s *Server) artifactReadKey(ctx context.Context, runID, sourceRunID, name string) (string, *objectErr) {
	source := runID
	if sourceRunID != "" && sourceRunID != runID {
		_, found, err := s.store.RunJobName(ctx, sourceRunID)
		if err != nil {
			return "", internalObject("source run lookup", sourceRunID, err)
		}
		if !found {
			return "", &objectErr{status: http.StatusNotFound, code: api.ObjectErrNotFound, msg: "source run not found"}
		}
		source = sourceRunID
	}
	key, err := artifact.ArtifactKey(source, name)
	if err != nil {
		return "", invalidObject("invalid artifact name")
	}
	return key, nil
}

func (s *Server) presignCache(ctx context.Context, rq objectRequester, req api.PresignRequest) (api.PresignResponse, *objectErr) {
	if req.Name != "" || req.SourceRunID != "" {
		return api.PresignResponse{}, invalidObject("name and sourceRunId apply to artifacts only")
	}
	if req.Key == "" {
		return api.PresignResponse{}, invalidObject("key is required")
	}
	// The namespace is the run record's job, never anything the client sends.
	job := rq.JobName
	switch req.Op {
	case api.ObjectOpGet, api.ObjectOpHead:
		dataKey, matched, info, err := cache.LookupExisting(ctx, s.objStore, s.presigner, job, req.Key, req.RestoreKeys)
		if errors.Is(err, cache.ErrCacheMiss) {
			return api.PresignResponse{}, &objectErr{status: http.StatusNotFound, code: api.ObjectErrCacheMiss, msg: "cache miss"}
		}
		if err != nil {
			return api.PresignResponse{}, internalObject("cache lookup", cache.JobPrefix(job), err)
		}
		pr, oerr := s.signOne(ctx, req.Op, dataKey, "")
		if oerr != nil {
			return api.PresignResponse{}, oerr
		}
		return api.PresignResponse{Request: pr, MatchedKey: matched, Size: info.Size, ETag: info.ETag}, nil
	case api.ObjectOpCommit:
		return s.commitCache(ctx, rq, req)
	}
	return s.presignKey(ctx, cache.DataKey(job, req.Key), req, "", api.ObjectErrCacheMiss)
}

// commitCache makes an uploaded archive visible to restores by writing its
// .meta. The size comes from the store and the expiry from the run's own
// spec, never from the client.
func (s *Server) commitCache(ctx context.Context, rq objectRequester, req api.PresignRequest) (api.PresignResponse, *objectErr) {
	dataKey := cache.DataKey(rq.JobName, req.Key)
	info, err := s.presigner.Stat(ctx, dataKey)
	if errors.Is(err, objectstore.ErrNotFound) {
		return api.PresignResponse{}, &objectErr{status: http.StatusNotFound, code: api.ObjectErrCacheMiss, msg: "no uploaded archive to commit"}
	}
	if err != nil {
		return api.PresignResponse{}, internalObject("stat", dataKey, err)
	}
	spec, err := s.store.GetRunSpec(ctx, rq.RunID)
	if err != nil {
		return api.PresignResponse{}, internalObject("load run spec", dataKey, err)
	}
	ttl, err := cacheTTLDays(spec, req.TTLDays)
	if err != nil {
		return api.PresignResponse{}, internalObject("parse run spec", dataKey, err)
	}
	expires := time.Now().Add(time.Duration(ttl) * 24 * time.Hour)
	if err := cache.Commit(ctx, s.objStore, rq.JobName, req.Key, info.Size, expires); err != nil {
		return api.PresignResponse{}, internalObject("commit", dataKey, err)
	}
	return api.PresignResponse{}, nil
}

// presignKey runs a non-lookup operation on an already-derived key.
// notFoundCode is the error code of a missing object for get/head.
func (s *Server) presignKey(ctx context.Context, key string, req api.PresignRequest, filename, notFoundCode string) (api.PresignResponse, *objectErr) {
	if strings.HasSuffix(key, ".meta") {
		// Unreachable with derived keys; a backstop for the .meta rule.
		return api.PresignResponse{}, invalidObject("refusing to sign a metadata object")
	}
	switch req.Op {
	case api.ObjectOpGet, api.ObjectOpHead:
		info, err := s.presigner.Stat(ctx, key)
		if errors.Is(err, objectstore.ErrNotFound) {
			return api.PresignResponse{}, &objectErr{status: http.StatusNotFound, code: notFoundCode, msg: "object not found"}
		}
		if err != nil {
			return api.PresignResponse{}, internalObject("stat", key, err)
		}
		pr, oerr := s.signOne(ctx, req.Op, key, filename)
		if oerr != nil {
			return api.PresignResponse{}, oerr
		}
		return api.PresignResponse{Request: pr, Size: info.Size, ETag: info.ETag}, nil
	case api.ObjectOpPut:
		pr, oerr := s.signOne(ctx, req.Op, key, "")
		if oerr != nil {
			return api.PresignResponse{}, oerr
		}
		return api.PresignResponse{Request: pr}, nil
	}

	// Multipart operations.
	if !s.presigner.MultipartPresign() {
		return api.PresignResponse{}, &objectErr{status: http.StatusNotImplemented, code: api.ObjectErrMultipartUnavailable, msg: "the object store does not accept presigned multipart uploads; use a single put"}
	}
	if req.Op != api.ObjectOpMultipartStart && req.UploadID == "" {
		return api.PresignResponse{}, invalidObject("uploadId is required")
	}
	switch req.Op {
	case api.ObjectOpMultipartStart:
		id, err := s.presigner.StartMultipart(ctx, key)
		if err != nil {
			return api.PresignResponse{}, internalObject("start multipart upload", key, err)
		}
		return api.PresignResponse{UploadID: id}, nil
	case api.ObjectOpMultipartParts:
		if len(req.PartNumbers) == 0 || len(req.PartNumbers) > api.MaxPresignPartsPerRequest {
			return api.PresignResponse{}, invalidObject("partNumbers must hold 1 to %d entries", api.MaxPresignPartsPerRequest)
		}
		expires := time.Now().Add(objectstore.PresignExpiry)
		parts := make([]api.PresignedRequest, 0, len(req.PartNumbers))
		for _, n := range req.PartNumbers {
			if n < 1 || n > api.MaxMultipartParts {
				return api.PresignResponse{}, invalidObject("part number %d is outside 1..%d", n, api.MaxMultipartParts)
			}
			u, err := s.presigner.PresignUploadPart(ctx, key, req.UploadID, n)
			if err != nil {
				return api.PresignResponse{}, internalObject("presign part", key, err)
			}
			parts = append(parts, api.PresignedRequest{Method: http.MethodPut, URL: u.String(), PartNumber: n, ExpiresAt: expires})
		}
		return api.PresignResponse{Parts: parts, UploadID: req.UploadID}, nil
	case api.ObjectOpMultipartComplete:
		if len(req.Parts) == 0 || len(req.Parts) > api.MaxMultipartParts {
			return api.PresignResponse{}, invalidObject("parts must hold 1 to %d entries", api.MaxMultipartParts)
		}
		parts := make([]objectstore.CompletedPart, 0, len(req.Parts))
		for _, p := range req.Parts {
			if p.PartNumber < 1 || p.PartNumber > api.MaxMultipartParts || p.ETag == "" {
				return api.PresignResponse{}, invalidObject("each part needs a partNumber in 1..%d and an etag", api.MaxMultipartParts)
			}
			parts = append(parts, objectstore.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag})
		}
		if err := s.presigner.CompleteMultipart(ctx, key, req.UploadID, parts); err != nil {
			return api.PresignResponse{}, internalObject("complete multipart upload", key, err)
		}
		return api.PresignResponse{UploadID: req.UploadID}, nil
	case api.ObjectOpMultipartAbort:
		if err := s.presigner.AbortMultipart(ctx, key, req.UploadID); err != nil {
			return api.PresignResponse{}, internalObject("abort multipart upload", key, err)
		}
		return api.PresignResponse{UploadID: req.UploadID}, nil
	}
	return api.PresignResponse{}, invalidObject("op %q does not apply here", req.Op)
}

// signOne presigns a single GET, HEAD or PUT of key.
func (s *Server) signOne(ctx context.Context, op, key, filename string) (*api.PresignedRequest, *objectErr) {
	if strings.HasSuffix(key, ".meta") {
		return nil, invalidObject("refusing to sign a metadata object")
	}
	expires := time.Now().Add(objectstore.PresignExpiry)
	var (
		u      *url.URL
		err    error
		method string
	)
	switch op {
	case api.ObjectOpGet:
		method = http.MethodGet
		u, err = s.presigner.PresignGet(ctx, key, filename)
	case api.ObjectOpHead:
		method = http.MethodHead
		u, err = s.presigner.PresignHead(ctx, key)
	case api.ObjectOpPut:
		method = http.MethodPut
		u, err = s.presigner.PresignPut(ctx, key)
	default:
		return nil, invalidObject("op %q cannot be signed", op)
	}
	if errors.Is(err, objectstore.ErrInvalidKey) {
		return nil, invalidObject("invalid object name")
	}
	if err != nil {
		return nil, internalObject("presign "+op, key, err)
	}
	return &api.PresignedRequest{Method: method, URL: u.String(), ExpiresAt: expires}, nil
}

// cacheTTLDays is the TTL a cache commit gets: the requested value, capped
// by the largest cache.ttlDays the run's own spec declares (steps, finally,
// and parallel groups). A declared 0 counts as the default, 30; nothing
// exceeds 365. A requested value of 0 or less means the declared maximum; a
// spec with no cache step allows the default.
func cacheTTLDays(spec []byte, requested int) (int, error) {
	var parsed dsl.Spec
	if len(spec) > 0 {
		if err := json.Unmarshal(spec, &parsed); err != nil {
			return 0, err
		}
	}
	maxDeclared := 0
	consider := func(c *dsl.CacheStep) {
		if c == nil {
			return
		}
		ttl := c.TTLDays
		if ttl <= 0 {
			ttl = defaultCacheTTLDays
		}
		maxDeclared = max(maxDeclared, min(ttl, maxCacheTTLDays))
	}
	for _, entries := range [][]dsl.StepEntry{parsed.Steps, parsed.Finally} {
		for _, e := range entries {
			consider(e.Cache)
			for _, p := range e.Parallel {
				consider(p.Cache)
			}
		}
	}
	if maxDeclared == 0 {
		maxDeclared = defaultCacheTTLDays
	}
	if requested <= 0 || requested > maxDeclared {
		return maxDeclared, nil
	}
	return requested, nil
}

// auditPresign writes an objects.presign row. resource names the object
// ("artifact:<run>/<name>" or "cache:<job>/<key>") or, for a denial, the run;
// details never carry a URL.
func (s *Server) auditPresign(r *http.Request, rq objectRequester, resource string, details map[string]any, status int) {
	if rq.PodUID != "" {
		if details == nil {
			details = map[string]any{}
		}
		details["podUid"] = rq.PodUID
	}
	s.auditAgentEvent(r, rq.AgentID, auditActionObjectsPresign, resource, status, details)
}
