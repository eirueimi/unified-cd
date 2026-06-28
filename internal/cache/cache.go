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
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// ErrCacheMiss is returned when no cache entry matches the key or restoreKeys.
var ErrCacheMiss = errors.New("cache miss")

// Meta holds cache entry metadata stored alongside the archive.
type Meta struct {
	OriginalKey string    `json:"originalKey"`
	ExpiresAt   time.Time `json:"expiresAt"`
	Size        int64     `json:"size"`
}

// objectKey converts a cache key to the MinIO object name prefix (without extension).
func objectKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return "caches/" + base64.RawURLEncoding.EncodeToString(h[:])
}

// Save compresses path as tar+zstd and stores it in store under key.
// A metadata object is stored alongside with TTL of ttlDays days.
func Save(ctx context.Context, store objectstore.ObjectStore, path, key string, ttlDays int) error {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)

	if err := filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("tar walk %q: %w", path, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close: %w", err)
	}

	archiveData := buf.Bytes()
	oKey := objectKey(key)
	if err := store.Put(ctx, oKey+".tar.zst", bytes.NewReader(archiveData), int64(len(archiveData))); err != nil {
		return fmt.Errorf("put archive: %w", err)
	}

	meta := Meta{
		OriginalKey: key,
		ExpiresAt:   time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour),
		Size:        int64(len(archiveData)),
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := store.Put(ctx, oKey+".meta", bytes.NewReader(metaData), int64(len(metaData))); err != nil {
		return fmt.Errorf("put meta: %w", err)
	}
	return nil
}

// Restore downloads and extracts the cache for key into path.
// If no exact match, tries restoreKeys prefix fallback.
// Returns (false, ErrCacheMiss) if nothing matches.
func Restore(ctx context.Context, store objectstore.ObjectStore, path, key string, restoreKeys []string) (bool, error) {
	oKey := objectKey(key)
	rc, err := store.Get(ctx, oKey+".tar.zst")
	if err == nil {
		defer rc.Close()
		if err := extract(rc, path); err != nil {
			return false, fmt.Errorf("extract: %w", err)
		}
		return true, nil
	}

	if len(restoreKeys) > 0 {
		fallbackKey, err := findBestMatch(ctx, store, restoreKeys)
		if err != nil || fallbackKey == "" {
			return false, ErrCacheMiss
		}
		rc, err := store.Get(ctx, fallbackKey+".tar.zst")
		if err != nil {
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

// findBestMatch scans all .meta objects and returns the object key (without extension)
// for the entry with the longest remaining TTL among those matching any prefix.
func findBestMatch(ctx context.Context, store objectstore.ObjectStore, prefixes []string) (string, error) {
	keys, err := store.List(ctx, "caches/")
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

	cleanDest := filepath.Clean(dest) + string(filepath.Separator)
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		target := filepath.Join(dest, filepath.FromSlash(hdr.Name))
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
