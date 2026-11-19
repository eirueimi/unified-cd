package objtransfer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// Artifacts returns an ObjectStore for artifact keys
// (artifacts/<run>/<name>.tar.gz) backed by presigned URLs. Put writes only to
// the client's own run; Get of another run's artifact sends sourceRunId.
// Delete and List are not supported: the presign endpoint has no such
// operations.
func (c *Client) Artifacts() objectstore.ObjectStore { return artifactStore{c} }

type artifactStore struct{ c *Client }

func (s artifactStore) Put(ctx context.Context, key string, content io.Reader, _ int64) error {
	runID, name, err := artifact.ParseKey(key)
	if err != nil {
		return err
	}
	if runID != s.c.RunID {
		return fmt.Errorf("artifact %q: cannot write to run %q from run %q", name, runID, s.c.RunID)
	}
	return s.c.upload(ctx, api.PresignRequest{Kind: api.ObjectKindArtifact, Name: name}, content)
}

func (s artifactStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	runID, name, err := artifact.ParseKey(key)
	if err != nil {
		return nil, err
	}
	req := api.PresignRequest{Kind: api.ObjectKindArtifact, Op: api.ObjectOpGet, Name: name}
	if runID != s.c.RunID {
		req.SourceRunID = runID
	}
	rc, _, err := s.c.openDownload(ctx, req)
	return rc, err
}

func (artifactStore) Delete(context.Context, string) error { return errors.ErrUnsupported }

func (artifactStore) List(context.Context, string) ([]string, error) {
	return nil, errors.ErrUnsupported
}

// Caches returns the cache.Remote of the client's run: the cache namespace is
// the run's job, which the controller reads from the run record.
func (c *Client) Caches() cache.Remote { return cacheRemote{c} }

type cacheRemote struct{ c *Client }

func (r cacheRemote) Open(ctx context.Context, key string, restoreKeys []string) (io.ReadCloser, string, error) {
	rc, resp, err := r.c.openDownload(ctx, api.PresignRequest{
		Kind: api.ObjectKindCache, Op: api.ObjectOpGet, Key: key, RestoreKeys: restoreKeys,
	})
	if err != nil {
		return nil, "", err
	}
	return rc, resp.MatchedKey, nil
}

// Save uploads the archive and then commits it, which is what makes the entry
// visible to restores.
func (r cacheRemote) Save(ctx context.Context, key string, ttlDays int, body io.Reader) error {
	tmpl := api.PresignRequest{Kind: api.ObjectKindCache, Key: key}
	if err := r.c.upload(ctx, tmpl, body); err != nil {
		return err
	}
	commit := withOp(tmpl, api.ObjectOpCommit)
	commit.TTLDays = ttlDays
	if _, err := r.c.presign(ctx, commit); err != nil {
		return fmt.Errorf("commit cache entry: %w", err)
	}
	return nil
}
