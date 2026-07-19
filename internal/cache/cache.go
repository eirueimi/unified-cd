package cache

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/klauspost/compress/zstd"
)

// ErrCacheMiss is returned when no cache entry matches the key or restoreKeys.
var ErrCacheMiss = errors.New("cache miss")

// Meta holds cache entry metadata stored alongside the archive.
type Meta struct {
	OriginalKey string    `json:"originalKey"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Size        int64     `json:"size"`

	// OwnerJob is the qualified job name (e.g. "team-a/build") that saved this
	// entry. It is redundant with the jobHash component of the object key
	// itself (objectKey/jobPrefix). On the exact-key restore path, isolation is
	// structural: the job hash is part of the object key. On the restoreKeys
	// fallback path, OwnerJob is checked against each candidate's Meta as
	// defense in depth (see findBestMatch) against a mis-keyed object landing in
	// another job's namespace by some path other than Save.
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

// jobPrefix returns the List prefix containing only this job's cache entries.
func jobPrefix(jobName string) string {
	j := sha256.Sum256([]byte(jobName))
	return "caches/" + base64.RawURLEncoding.EncodeToString(j[:]) + "/"
}

// Save compresses path as tar+zstd and stores it in store under key, streaming
// the archive so it is never fully buffered in memory. A metadata object is
// stored alongside with TTL of ttlDays days; its Size is the number of bytes
// streamed to the store, captured during the upload. jobName is the qualified
// job name that owns this entry (see objectKey) and is recorded in Meta.OwnerJob.
//
// jobName must be non-empty: sha256("") is just as valid a namespace as any
// other hash, so an empty jobName would silently recreate the pre-fix global
// cache namespace (any job could plant or read another job's entries) instead
// of failing loudly. The sidecar CLI already rejects an empty --job flag
// before it ever reaches here (see cmd/unified-sidecar/run.go); this is the
// same invariant enforced where it actually lives, so no other caller of this
// library can bypass it by skipping the CLI.
func Save(ctx context.Context, store objectstore.ObjectStore, jobName, path, key string, ttlDays int) error {
	if jobName == "" {
		return fmt.Errorf("cache save: jobName must not be empty")
	}
	oKey := objectKey(jobName, key)

	body := artifact.StreamTarZstd(path)
	counter := &countingReader{r: body}
	if err := store.Put(ctx, oKey+".tar.zst", counter, -1); err != nil {
		body.Close()
		return fmt.Errorf("put archive: %w", err)
	}
	body.Close()

	meta := Meta{
		OriginalKey: key,
		ExpiresAt:   time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour),
		Size:        counter.n,
		OwnerJob:    jobName,
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := store.Put(ctx, oKey+".meta", bytes.NewReader(metaData), int64(len(metaData))); err != nil {
		// The archive object was already written; without its .meta it is
		// invisible to both lookup and GC (which iterate .meta only), so it
		// would leak forever. Compensate best-effort, like the log archiver
		// does on CreateLogArchive failure.
		if derr := store.Delete(ctx, oKey+".tar.zst"); derr != nil {
			slog.Warn("cache save: cleanup of orphaned archive failed", "key", oKey, "error", derr)
		}
		return fmt.Errorf("put meta: %w", err)
	}
	return nil
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
// Returns (false, ErrCacheMiss) if nothing matches. jobName is the qualified
// job name that must own the entry being restored (see objectKey/jobPrefix):
// it is namespaced into the exact-key lookup and, for the restoreKeys
// fallback, both bounds the candidates enumerated (findBestMatch only lists
// this job's own prefix) and is checked again against each candidate's
// Meta.OwnerJob as defense in depth.
//
// jobName must be non-empty for the same reason as in Save: sha256("") is a
// valid namespace like any other, so an empty jobName would silently read
// from the pre-fix global namespace instead of failing loudly.
func Restore(ctx context.Context, store objectstore.ObjectStore, jobName, path, key string, restoreKeys []string) (bool, error) {
	if jobName == "" {
		return false, fmt.Errorf("cache restore: jobName must not be empty")
	}
	oKey := objectKey(jobName, key)
	rc, err := store.Get(ctx, oKey+".tar.zst")
	if err == nil {
		defer rc.Close()
		if err := extract(rc, path); err != nil {
			return false, fmt.Errorf("extract: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, objectstore.ErrNotFound) {
		// A non-NotFound error (e.g. a transient network failure) must not be
		// silently swallowed into a restoreKeys fallback attempt — that would
		// make a transient failure indistinguishable from a genuine miss.
		return false, fmt.Errorf("get cache object: %w", err)
	}

	if len(restoreKeys) > 0 {
		fallbackKey, err := findBestMatch(ctx, store, jobName, restoreKeys)
		if err != nil || fallbackKey == "" {
			return false, ErrCacheMiss
		}
		rc, err := store.Get(ctx, fallbackKey+".tar.zst")
		if err != nil {
			if !errors.Is(err, objectstore.ErrNotFound) {
				return false, fmt.Errorf("get fallback cache object: %w", err)
			}
			return false, ErrCacheMiss
		}
		defer rc.Close()
		if err := extract(rc, path); err != nil {
			return false, fmt.Errorf("extract fallback: %w", err)
		}
		return true, nil
	}

	return false, ErrCacheMiss
}

// DeleteExpired removes all cache entries whose ExpiresAt is before past.
// Returns the count of deleted entries.
func DeleteExpired(ctx context.Context, store objectstore.ObjectStore, past time.Time) (int, error) {
	keys, err := store.List(ctx, "caches/")
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	deleted := 0
	for _, k := range keys {
		if !strings.HasSuffix(k, ".meta") {
			continue
		}
		rc, err := store.Get(ctx, k)
		if err != nil {
			continue
		}
		var m Meta
		if err := json.NewDecoder(rc).Decode(&m); err != nil {
			rc.Close()
			continue
		}
		rc.Close()

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
	return deleted, nil
}

// findBestMatch scans this job's .meta objects (and only this job's — the
// List call is scoped to jobPrefix(jobName), so another job's entries are
// never even enumerated, let alone selected) and returns the object key
// (without extension) for the entry with the longest remaining TTL among
// those matching any prefix. Any candidate whose Meta.OwnerJob does not match
// jobName is skipped as defense in depth against a mis-keyed object.
func findBestMatch(ctx context.Context, store objectstore.ObjectStore, jobName string, prefixes []string) (string, error) {
	keys, err := store.List(ctx, jobPrefix(jobName))
	if err != nil {
		return "", err
	}
	var best *Meta
	var bestKey string
	now := time.Now()
	for _, k := range keys {
		if !strings.HasSuffix(k, ".meta") {
			continue
		}
		rc, err := store.Get(ctx, k)
		if err != nil {
			continue
		}
		var m Meta
		if err := json.NewDecoder(rc).Decode(&m); err != nil {
			rc.Close()
			continue
		}
		rc.Close()
		if m.ExpiresAt.Before(now) {
			continue
		}
		if m.OwnerJob != jobName {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(m.OriginalKey, prefix) {
				if best == nil || m.ExpiresAt.After(best.ExpiresAt) {
					best = &m
					bestKey = strings.TrimSuffix(k, ".meta")
				}
				break
			}
		}
	}
	return bestKey, nil
}

// extract decompresses a tar+zstd stream into dest.
// Rejects paths that escape dest (path traversal protection).
func extract(r io.Reader, dest string) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	// Resolve dest to absolute so the traversal guard works for a relative dest
	// (e.g. ".") against archives with relative entries (tar -C dir .).
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("resolve dest %q: %w", dest, err)
	}
	cleanDest := absDest + string(filepath.Separator)
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		target := filepath.Join(absDest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target+string(filepath.Separator), cleanDest) {
			return fmt.Errorf("invalid path %q in cache archive", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
