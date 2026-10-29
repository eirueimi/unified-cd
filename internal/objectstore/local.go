package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LocalObjectStore implements ObjectStore using the local filesystem. For dev/test.
type LocalObjectStore struct {
	baseDir string
}

func NewLocalObjectStore(baseDir string) *LocalObjectStore {
	return &LocalObjectStore{baseDir: baseDir}
}

func (s *LocalObjectStore) Put(_ context.Context, key string, content io.Reader, _ int64) error {
	path := filepath.Join(s.baseDir, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// Write to a temp file and rename it into place, so a body that fails
	// halfway (an archiver that hit a permission error) never leaves a
	// truncated object under key, nor destroys the one that was there before.
	f, err := os.CreateTemp(filepath.Dir(path), localTempPrefix)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	tmp := f.Name()
	if _, err := io.Copy(f, content); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// localTempPrefix names Put's in-flight temp files; List hides them.
const localTempPrefix = ".put-"

// Get implements ObjectStore.Get, including the ErrNotFound contract: os.Open
// already fails eagerly (not lazily) for a missing file, so this only needs
// to translate that into the shared sentinel that S3ObjectStore also uses,
// so fakes/tests backed by LocalObjectStore match real S3 behavior.
func (s *LocalObjectStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path := filepath.Join(s.baseDir, filepath.FromSlash(key))
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("open %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("open %q: %w", key, err)
	}
	return f, nil
}

func (s *LocalObjectStore) Delete(_ context.Context, key string) error {
	path := filepath.Join(s.baseDir, filepath.FromSlash(key))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *LocalObjectStore) List(_ context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(s.baseDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.baseDir, p)
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), localTempPrefix) {
			return nil
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // baseDir not yet created → empty list
		}
		return nil, fmt.Errorf("list %q: %w", s.baseDir, err)
	}
	return keys, nil
}
