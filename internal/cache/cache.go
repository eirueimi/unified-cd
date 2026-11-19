package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// ErrCacheMiss is returned when no cache entry matches the key or restoreKeys.
var ErrCacheMiss = errors.New("cache miss")

// OrphanGrace is how long a data archive may exist without a .meta before cache
// GC removes it. It covers an upload that is still in flight (or about to be
// committed) as well as one that never completed.
const OrphanGrace = 24 * time.Hour

// Meta holds cache entry metadata stored alongside the archive.
type Meta struct {
	OriginalKey string    `json:"originalKey"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Size        int64     `json:"size"`

	// OwnerJob is the qualified job name (e.g. "team-a/build") that saved this
	// entry. It is redundant with the jobHash component of the object key
	// itself (objectKey/JobPrefix). On the exact-key path, isolation is
	// structural: the job hash is part of the object key. OwnerJob is checked
	// against each candidate's Meta as defense in depth (see Lookup and
	// findBestMatch) against a mis-keyed object landing in another job's
	// namespace by some path other than Save.
	OwnerJob string `json:"ownerJob"`
}

// objectKey converts a job name + cache key to the object name prefix (without
// extension). The job component namespaces every entry: without it the cache is
// one flat global namespace, and a job could plant an entry that another job's
// restoreKeys prefix-match would select and execute. Job identity (not run ID)
// is the right granularity — reuse across runs of the same job is the point of
// a cache.
func objectKey(jobName, key string) string {
	j := sha256.Sum256([]byte(jobName))
	h := sha256.Sum256([]byte(key))
	return "caches/" + base64.RawURLEncoding.EncodeToString(j[:]) + "/" + base64.RawURLEncoding.EncodeToString(h[:])
}

// DataKey returns the object key of the archive for (jobName, key).
func DataKey(jobName, key string) string { return objectKey(jobName, key) + ".tar.zst" }

func metaKey(jobName, key string) string { return objectKey(jobName, key) + ".meta" }

// JobPrefix returns the List prefix containing only this job's cache entries.
func JobPrefix(jobName string) string {
	j := sha256.Sum256([]byte(jobName))
	return "caches/" + base64.RawURLEncoding.EncodeToString(j[:]) + "/"
}

// Remote is where a cache entry is read from and written to. StoreRemote is the
// object-store implementation; the controller decides matches and writes .meta.
type Remote interface {
	// Open returns the archive for key, falling back to the longest-lived entry
	// matching one of restoreKeys. It returns ErrCacheMiss when nothing matches.
	// matchedKey is the original key of the entry that was opened.
	Open(ctx context.Context, key string, restoreKeys []string) (rc io.ReadCloser, matchedKey string, err error)
	// Save streams body as the archive for key and records its metadata with a
	// TTL of ttlDays days.
	Save(ctx context.Context, key string, ttlDays int, body io.Reader) error
}

// StoreRemote is a Remote backed directly by an object store. JobName is the
// qualified job name that owns the entries; it must be non-empty (sha256("") is
// as valid a namespace as any other, so an empty name would silently recreate a
// global cache namespace instead of failing loudly).
type StoreRemote struct {
	Store   objectstore.ObjectStore
	JobName string
}

// Open implements Remote.
func (r *StoreRemote) Open(ctx context.Context, key string, restoreKeys []string) (io.ReadCloser, string, error) {
	dataKey, matched, err := Lookup(ctx, r.Store, r.JobName, key, restoreKeys)
	if err != nil {
		return nil, "", err
	}
	rc, err := r.Store.Get(ctx, dataKey)
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return nil, "", ErrCacheMiss
		}
		return nil, "", fmt.Errorf("get cache object: %w", err)
	}
	return rc, matched, nil
}

// Save implements Remote: the archive is put first, then .meta is committed
// with the number of bytes streamed.
func (r *StoreRemote) Save(ctx context.Context, key string, ttlDays int, body io.Reader) error {
	if r.JobName == "" {
		return fmt.Errorf("cache save: jobName must not be empty")
	}
	counter := &countingReader{r: body}
	if err := r.Store.Put(ctx, DataKey(r.JobName, key), counter, -1); err != nil {
		return fmt.Errorf("put archive: %w", err)
	}
	expires := time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour)
	if err := Commit(ctx, r.Store, r.JobName, key, counter.n, expires); err != nil {
		// The archive was written but has no .meta; remove it best-effort (GC
		// would also remove it after OrphanGrace).
		if derr := r.Store.Delete(ctx, DataKey(r.JobName, key)); derr != nil {
			slog.Warn("cache save: cleanup of orphaned archive failed", "key", objectKey(r.JobName, key), "error", derr)
		}
		return err
	}
	return nil
}

// Commit writes the .meta object that makes the archive for (jobName, key)
// visible to Lookup.
func Commit(ctx context.Context, store objectstore.ObjectStore, jobName, key string, size int64, expiresAt time.Time) error {
	if jobName == "" {
		return fmt.Errorf("cache save: jobName must not be empty")
	}
	meta := Meta{OriginalKey: key, ExpiresAt: expiresAt, Size: size, OwnerJob: jobName}
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := store.Put(ctx, metaKey(jobName, key), bytes.NewReader(data), int64(len(data))); err != nil {
		return fmt.Errorf("put meta: %w", err)
	}
	return nil
}

// readMeta reads and decodes the .meta object at k.
func readMeta(ctx context.Context, store objectstore.ObjectStore, k string) (Meta, error) {
	rc, err := store.Get(ctx, k)
	if err != nil {
		return Meta{}, err
	}
	defer rc.Close()
	var m Meta
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		return Meta{}, fmt.Errorf("decode meta: %w", err)
	}
	return m, nil
}

// Lookup resolves key (then restoreKeys) to the archive object to read. An
// exact hit needs a .meta whose OwnerJob is jobName (expiry is not checked); a
// data object without .meta is not a hit. The restoreKeys scan picks the
// unexpired entry with the longest remaining TTL whose original key has one of
// the prefixes. It returns ErrCacheMiss when nothing matches; matchedKey is the
// matched entry's original key.
func Lookup(ctx context.Context, store objectstore.ObjectStore, jobName, key string, restoreKeys []string) (dataKey, matchedKey string, err error) {
	if jobName == "" {
		return "", "", fmt.Errorf("cache restore: jobName must not be empty")
	}
	m, err := readMeta(ctx, store, metaKey(jobName, key))
	switch {
	case err == nil:
		if m.OwnerJob == jobName {
			return DataKey(jobName, key), key, nil
		}
	case errors.Is(err, objectstore.ErrNotFound):
		// No metadata: not an exact hit; fall through to restoreKeys.
	default:
		// A transient failure (e.g. a network error) must not be folded into a
		// restoreKeys fallback: it would be indistinguishable from a genuine miss.
		return "", "", fmt.Errorf("get cache meta: %w", err)
	}

	if len(restoreKeys) > 0 {
		best, bestKey, err := findBestMatch(ctx, store, jobName, restoreKeys)
		if err != nil || best == nil {
			return "", "", ErrCacheMiss
		}
		return bestKey + ".tar.zst", best.OriginalKey, nil
	}
	return "", "", ErrCacheMiss
}

// LookupExisting is Lookup for a caller that hands out a URL to the archive
// instead of opening it, so a dead URL must never be returned: each candidate's
// archive is checked with st.Stat. An exact hit whose archive is missing falls
// back to the restoreKeys scan, and the scan skips entries whose archive is
// missing. It returns the archive's ObjectInfo along with the keys, or
// ErrCacheMiss.
func LookupExisting(ctx context.Context, store objectstore.ObjectStore, st objectstore.Statter, jobName, key string, restoreKeys []string) (dataKey, matchedKey string, info objectstore.ObjectInfo, err error) {
	if jobName == "" {
		return "", "", objectstore.ObjectInfo{}, fmt.Errorf("cache restore: jobName must not be empty")
	}
	stat := func(k string) (objectstore.ObjectInfo, bool, error) {
		info, err := st.Stat(ctx, k)
		if errors.Is(err, objectstore.ErrNotFound) {
			return objectstore.ObjectInfo{}, false, nil
		}
		if err != nil {
			return objectstore.ObjectInfo{}, false, fmt.Errorf("stat cache object: %w", err)
		}
		return info, true, nil
	}
	m, err := readMeta(ctx, store, metaKey(jobName, key))
	switch {
	case err == nil:
		if m.OwnerJob == jobName {
			info, ok, err := stat(DataKey(jobName, key))
			if err != nil {
				return "", "", objectstore.ObjectInfo{}, err
			}
			if ok {
				return DataKey(jobName, key), key, info, nil
			}
		}
	case errors.Is(err, objectstore.ErrNotFound):
	default:
		return "", "", objectstore.ObjectInfo{}, fmt.Errorf("get cache meta: %w", err)
	}
	if len(restoreKeys) == 0 {
		return "", "", objectstore.ObjectInfo{}, ErrCacheMiss
	}
	cands, err := matchCandidates(ctx, store, jobName, restoreKeys)
	if err != nil {
		return "", "", objectstore.ObjectInfo{}, ErrCacheMiss
	}
	for _, c := range cands {
		info, ok, err := stat(c.stem + ".tar.zst")
		if err != nil {
			return "", "", objectstore.ObjectInfo{}, err
		}
		if ok {
			return c.stem + ".tar.zst", c.meta.OriginalKey, info, nil
		}
	}
	return "", "", objectstore.ObjectInfo{}, ErrCacheMiss
}

// Save compresses path as tar+zstd and stores it through remote under key,
// streaming the archive so it is never fully buffered in memory. A metadata
// object is stored alongside with TTL of ttlDays days; its Size is the number
// of bytes streamed, captured during the upload.
func Save(ctx context.Context, remote Remote, path, key string, ttlDays int) error {
	return SaveExcluding(ctx, remote, path, key, ttlDays, nil)
}

// SaveExcluding is Save that leaves the paths in exclude out of the archive
// (see artifact.WriteTarZstdExcluding).
func SaveExcluding(ctx context.Context, remote Remote, path, key string, ttlDays int, exclude []string) error {
	return save(ctx, remote, key, ttlDays, func() io.ReadCloser {
		return artifact.StreamTarZstdExcluding(path, exclude)
	})
}

// SaveIn is SaveExcluding confined to root: path must lie at or below root and
// nothing outside root is read, whatever symlinks the tree holds (see
// artifact.WriteTarZstdIn). It is what the k8s sidecar runs.
func SaveIn(ctx context.Context, remote Remote, root, path, key string, ttlDays int, exclude []string) error {
	return save(ctx, remote, key, ttlDays, func() io.ReadCloser {
		return artifact.StreamTarZstdIn(root, path, exclude)
	})
}

func save(ctx context.Context, remote Remote, key string, ttlDays int, open func() io.ReadCloser) error {
	body := open()
	defer body.Close()
	return remote.Save(ctx, key, ttlDays, body)
}

// countingReader counts the bytes read through it — i.e. the bytes the object
// store consumed from the archive stream — so Save can record Meta.Size
// without buffering the whole archive to measure it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// Restore downloads and extracts the cache for key into path.
// If no exact match, tries restoreKeys prefix fallback.
// Returns (false, ErrCacheMiss) if nothing matches. The remote decides what
// matches (see Lookup).
func Restore(ctx context.Context, remote Remote, path, key string, restoreKeys []string) (bool, error) {
	return RestoreExcluding(ctx, remote, path, key, restoreKeys, nil)
}

// RestoreExcluding is Restore that does not extract an entry at or below any of
// the paths in exclude (absolute, in the same form as path). It is the
// counterpart of SaveExcluding: an archive written before a directory became a
// volume's mount point still holds files there, and extracting them would write
// into the volume.
func RestoreExcluding(ctx context.Context, remote Remote, path, key string, restoreKeys []string, exclude []string) (bool, error) {
	hit, _, err := RestoreWritten(ctx, remote, path, key, restoreKeys, exclude, nil)
	return hit, err
}

// RestoreWritten is RestoreExcluding that records what it extracted in w (nil
// records nothing), so the caller can re-own exactly that. Symlink and hardlink
// entries in the archive are skipped, as RestoreIn does; skippedLinks is how
// many were. It is what the host agent runs.
func RestoreWritten(ctx context.Context, remote Remote, path, key string, restoreKeys []string, exclude []string, w *artifact.Written) (hit bool, skippedLinks int, err error) {
	hit, _, err = restore(ctx, remote, key, restoreKeys, func(r io.Reader) error {
		n, err := artifact.ExtractTarZstdDir(r, path, exclude, false, w)
		skippedLinks = n
		return err
	})
	return hit, skippedLinks, err
}

// RestoreInWritten is RestoreIn that also records what it extracted in w (nil
// records nothing), creating path under root when it is missing. It is what the
// host agent runs for an isolated claim.
func RestoreInWritten(ctx context.Context, remote Remote, root, path, key string, restoreKeys []string, exclude []string, w *artifact.Written) (hit bool, skippedLinks int, err error) {
	hit, _, err = restore(ctx, remote, key, restoreKeys, func(r io.Reader) error {
		n, err := artifact.ExtractTarZstdDirIn(r, root, path, exclude, false, w)
		skippedLinks = n
		return err
	})
	return hit, skippedLinks, err
}

// RestoreIn is RestoreExcluding confined to root: path must lie at or below
// root and nothing is written through a symlink that leads outside root (see
// artifact.ExtractTarZstdIn). Symlink and hardlink entries in the archive are
// skipped; skippedLinks is how many were, so the caller can warn that the
// restored tree may be incomplete. matchedKey is the original key of the entry
// that was restored. It is what the k8s sidecar runs.
func RestoreIn(ctx context.Context, remote Remote, root, path, key string, restoreKeys []string, exclude []string) (hit bool, matchedKey string, skippedLinks int, err error) {
	hit, matchedKey, err = restore(ctx, remote, key, restoreKeys, func(r io.Reader) error {
		n, err := artifact.ExtractTarZstdInCounting(r, root, path, exclude, false)
		skippedLinks = n
		return err
	})
	return hit, matchedKey, skippedLinks, err
}

func restore(ctx context.Context, remote Remote, key string, restoreKeys []string, extract func(io.Reader) error) (bool, string, error) {
	rc, matched, err := remote.Open(ctx, key, restoreKeys)
	if err != nil {
		return false, "", err
	}
	defer rc.Close()
	if err := extract(rc); err != nil {
		return false, "", fmt.Errorf("extract: %w", err)
	}
	return true, matched, nil
}

// DeleteExpired removes all cache entries whose ExpiresAt is before past, and
// (when the store supports Stat) data archives that have had no .meta for longer
// than OrphanGrace. Returns the count of deleted entries.
func DeleteExpired(ctx context.Context, store objectstore.ObjectStore, past time.Time) (int, error) {
	keys, err := store.List(ctx, "caches/")
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	deleted := 0
	metas := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		if strings.HasSuffix(k, ".meta") {
			metas[strings.TrimSuffix(k, ".meta")] = struct{}{}
		}
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, ".meta") {
			continue
		}
		m, err := readMeta(ctx, store, k)
		if err != nil {
			continue
		}
		if m.ExpiresAt.Before(past) {
			archiveKey := strings.TrimSuffix(k, ".meta") + ".tar.zst"
			if err := store.Delete(ctx, archiveKey); err != nil {
				continue
			}
			if err := store.Delete(ctx, k); err != nil {
				continue
			}
			deleted++
		}
	}

	statter, ok := store.(objectstore.Statter)
	if !ok {
		return deleted, nil
	}
	cutoff := past.Add(-OrphanGrace)
	for _, k := range keys {
		if !strings.HasSuffix(k, ".tar.zst") {
			continue
		}
		if _, has := metas[strings.TrimSuffix(k, ".tar.zst")]; has {
			continue
		}
		info, err := statter.Stat(ctx, k)
		if err != nil {
			continue
		}
		if info.LastModified.Before(cutoff) {
			if err := store.Delete(ctx, k); err != nil {
				continue
			}
			deleted++
		}
	}
	return deleted, nil
}

// findBestMatch scans this job's .meta objects (and only this job's: the List
// call is scoped to JobPrefix(jobName), so another job's entries are never even
// enumerated, let alone selected) and returns the entry with the longest
// remaining TTL among those matching any prefix, with its object key (without
// extension). Any candidate whose Meta.OwnerJob does not match jobName is
// skipped as defense in depth against a mis-keyed object. It returns a nil
// Meta when nothing matches.
func findBestMatch(ctx context.Context, store objectstore.ObjectStore, jobName string, prefixes []string) (*Meta, string, error) {
	cands, err := matchCandidates(ctx, store, jobName, prefixes)
	if err != nil || len(cands) == 0 {
		return nil, "", err
	}
	return &cands[0].meta, cands[0].stem, nil
}

type candidate struct {
	meta Meta
	stem string // object key without extension
}

// matchCandidates returns every unexpired entry of jobName whose original key
// has one of prefixes, longest remaining TTL first (see findBestMatch).
func matchCandidates(ctx context.Context, store objectstore.ObjectStore, jobName string, prefixes []string) ([]candidate, error) {
	keys, err := store.List(ctx, JobPrefix(jobName))
	if err != nil {
		return nil, err
	}
	var out []candidate
	now := time.Now()
	for _, k := range keys {
		if !strings.HasSuffix(k, ".meta") {
			continue
		}
		m, err := readMeta(ctx, store, k)
		if err != nil {
			continue
		}
		if m.ExpiresAt.Before(now) || m.OwnerJob != jobName {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(m.OriginalKey, prefix) {
				out = append(out, candidate{meta: m, stem: strings.TrimSuffix(k, ".meta")})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].meta.ExpiresAt.After(out[j].meta.ExpiresAt) })
	return out, nil
}
