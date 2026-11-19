package api

import "time"

// Object kinds and operations accepted by POST /api/v1/runs/{runID}/objects/presign.
const (
	ObjectKindArtifact = "artifact"
	ObjectKindCache    = "cache"

	ObjectOpGet               = "get"
	ObjectOpHead              = "head"
	ObjectOpPut               = "put"
	ObjectOpMultipartStart    = "multipart-start"
	ObjectOpMultipartParts    = "multipart-parts"
	ObjectOpMultipartComplete = "multipart-complete"
	ObjectOpMultipartAbort    = "multipart-abort"
	ObjectOpCommit            = "commit"
)

// Presign response modes.
const (
	// ObjectModeDirect: the response carries presigned URLs for the store.
	ObjectModeDirect = "direct"
	// ObjectModeController: the store cannot presign (the local filesystem
	// store); the client must stream through the controller instead.
	ObjectModeController = "controller"
)

// Error codes of ObjectError.
const (
	ObjectErrInvalid              = "invalid"
	ObjectErrUnauthenticated      = "unauthenticated"
	ObjectErrForbidden            = "forbidden"
	ObjectErrNotFound             = "not-found"
	ObjectErrCacheMiss            = "cache-miss"
	ObjectErrBindingPending       = "binding-pending"
	ObjectErrRateLimited          = "rate-limited"
	ObjectErrMultipartUnavailable = "multipart-unavailable"
	ObjectErrNoObjectStore        = "no-object-store"
	ObjectErrUnavailable          = "unavailable"
	ObjectErrInternal             = "internal"
)

// MaxPresignPartsPerRequest bounds PresignRequest.PartNumbers.
const MaxPresignPartsPerRequest = 100

// MaxMultipartParts is the S3 limit on parts in one multipart upload; part
// numbers are 1..MaxMultipartParts.
const MaxMultipartParts = 10000

// PresignRequest is the body of POST /api/v1/runs/{runID}/objects/presign.
// The client never sends an object key: the controller derives it from the
// run in the path (artifacts) or the run record's job name (caches).
type PresignRequest struct {
	Kind        string       `json:"kind"`                  // "artifact" | "cache"
	Op          string       `json:"op"`                    // get|head|put|multipart-start|multipart-parts|multipart-complete|multipart-abort|commit
	Name        string       `json:"name,omitempty"`        // artifact name
	SourceRunID string       `json:"sourceRunId,omitempty"` // artifact get/head from another run
	Key         string       `json:"key,omitempty"`         // cache key as written in the job (unhashed)
	RestoreKeys []string     `json:"restoreKeys,omitempty"` // cache get/head
	UploadID    string       `json:"uploadId,omitempty"`
	PartNumbers []int        `json:"partNumbers,omitempty"` // 1..10000, at most 100 per request
	Parts       []ObjectPart `json:"parts,omitempty"`       // multipart-complete
	TTLDays     int          `json:"ttlDays,omitempty"`     // cache commit
}

// ObjectPart names one uploaded part of a multipart upload.
type ObjectPart struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}

// PresignedRequest is one HTTP request the client sends to the store as-is.
// URL is a bearer credential: never log it.
type PresignedRequest struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers,omitempty"` // must be sent as-is
	PartNumber int               `json:"partNumber,omitempty"`
	ExpiresAt  time.Time         `json:"expiresAt"`
}

// PresignResponse is the answer to a PresignRequest.
type PresignResponse struct {
	Mode       string             `json:"mode"` // "direct" | "controller"
	Request    *PresignedRequest  `json:"request,omitempty"`
	Parts      []PresignedRequest `json:"parts,omitempty"`
	UploadID   string             `json:"uploadId,omitempty"`
	MatchedKey string             `json:"matchedKey,omitempty"` // cache get/head: the entry's original key
	Size       int64              `json:"size,omitempty"`       // get/head
	ETag       string             `json:"etag,omitempty"`       // get/head
}

// ObjectError is the error body of the presign endpoint.
type ObjectError struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
