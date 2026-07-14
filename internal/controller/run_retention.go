package controller

import (
	"context"
	"fmt"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// deleteRunEverywhere removes a run's object-store data and then its DB row.
// Object deletion goes first so a surviving DB row always still references
// any surviving objects: a failure leaves the run intact for a later retry,
// never an orphaned object. ObjectStore.Delete is nil for missing keys, so
// retries after a partial failure are idempotent. Both the retention sweeper
// and the manual DELETE /runs/{id} handler use this helper. A nil obj
// (object store not configured) skips object deletion — nothing was ever
// uploaded in such deployments.
func deleteRunEverywhere(ctx context.Context, st store.Store, obj objectstore.ObjectStore, runID string) error {
	if obj != nil {
		arch, err := st.GetLogArchive(ctx, runID)
		if err != nil {
			return fmt.Errorf("get log archive: %w", err)
		}
		if arch != nil {
			if err := obj.Delete(ctx, arch.ObjectKey); err != nil {
				return fmt.Errorf("delete log archive object %s: %w", arch.ObjectKey, err)
			}
		}
		keys, err := obj.List(ctx, "artifacts/"+runID+"/")
		if err != nil {
			return fmt.Errorf("list artifact objects: %w", err)
		}
		for _, key := range keys {
			if err := obj.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete artifact object %s: %w", key, err)
			}
		}
	}
	return st.DeleteRun(ctx, runID)
}
