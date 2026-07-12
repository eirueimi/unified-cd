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
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, content); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

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
