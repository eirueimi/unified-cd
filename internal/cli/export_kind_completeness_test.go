package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/schemakinds"
)

// TestExportKindDir_CoversAllSchemaRootKinds guards exportKindDir against the
// failure mode described at its declaration: a manifest kind that exists but
// that exportKindDir was never taught about, so `export` silently omits it
// from a backup. That backup then looks complete — the command prints
// "exported N resources" and exits 0 — and the gap is only found later, at
// restore, when the resource that was never written is simply not there.
//
// This repository has now shipped that exact shape of bug — a hand-maintained
// list quietly drifting from the thing it is supposed to describe — eight
// times: `uses:` inlining silently dropping a shared template's approval
// gate, stepToStepEntry, safeStepCtx.snapshot(), the CEL declaration/
// activation pair, ExpandAgentSelector/ExpandConcurrency, the generators'
// root kinds, an earlier form of export itself, and the metrics wiring test.
// It was raised again during PR #156's review against exportKindDir
// specifically, and deliberately left, on the grounds that reflection alone
// cannot enumerate manifest kinds. That objection is correct about
// reflection but beside the point: derivation can, via the generated JSON
// Schema (schemas/unified-cd.schema.json), which schemagen writes one
// document-level oneOf branch into per root kind and which TestSchemaIsUpToDate
// (cmd/schemagen/main_test.go) keeps from ever drifting from internal/dsl.
// schemakinds.RootKinds reads that oneOf back out — the same derivation
// cmd/docgen already relies on for the field reference's table of contents.
//
// So this test contains no list of its own. It asks the schema what kinds
// exist and checks each one (other than the deliberate exceptions below)
// against exportKindDir, rather than restating export's own idea of what it
// covers and comparing that idea to itself.
func TestExportKindDir_CoversAllSchemaRootKinds(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "schemas", "unified-cd.schema.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	rootKinds, err := schemakinds.RootKinds(schema)
	if err != nil {
		t.Fatalf("derive root kinds: %v", err)
	}

	// Kinds that are legitimately absent from exportKindDir, and why. Both are
	// structural, not oversights, and both need to stay named here so the
	// guard doesn't mistake "known to be handled elsewhere" for "forgotten".
	exempt := map[string]string{
		// Job is exported too, but through the qualified-path branch in
		// runExport (job.Path/job.Leaf + ".yaml"), not through exportKindDir —
		// exportKindDir is documented above as covering non-Job kinds, and a
		// Job entry in it would in fact be wrong: jobs are not written under a
		// fixed top-level directory the way every other kind is.
		"Job": "exported via its qualified path, not exportKindDir (see runExport)",

		// JobTemplate has no controller-side list endpoint at all: unlike
		// every other root kind, a JobTemplate is never stored in the
		// controller's own store. It lives in a git template repository and is
		// resolved by path by internal/gittemplate when a Job's `uses:` refers
		// to it (see internal/gittemplate/resolve.go), then inlined into the
		// referencing Job's spec at parse time. There is nothing for `export`
		// (which mirrors controller-held state via /api/v1/*) to list, so it
		// structurally cannot appear in exportKindDir the way Schedule,
		// WebhookReceiver, GitCredential, AppSource, and Vars do.
		"JobTemplate": "resolved from a git template repo at parse time, not stored or listed by the controller",
	}

	var missing []string
	for _, kind := range rootKinds {
		if _, ok := exempt[kind]; ok {
			continue
		}
		if _, ok := exportKindDir[kind]; !ok {
			missing = append(missing, kind)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("kind(s) %s exist in the schema but have no entry in exportKindDir: "+
			"`export` will silently omit them from every backup until this is fixed",
			strings.Join(missing, ", "))
	}

	// The reverse direction: an exportKindDir entry for a kind the schema no
	// longer recognises as a root would mean export is writing a directory for
	// something that isn't a manifest kind anymore — equally worth catching,
	// and free to check here since rootKinds is already in hand.
	known := map[string]bool{}
	for _, kind := range rootKinds {
		known[kind] = true
	}
	var stale []string
	for kind := range exportKindDir {
		if !known[kind] {
			stale = append(stale, kind)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("exportKindDir has entry/entries for kind(s) %s, which the schema no longer lists as a root kind",
			strings.Join(stale, ", "))
	}
}

// repoRoot locates the repository root from this test file's own path, the
// same runtime.Caller pattern internal/config's tests use to reach files
// (like manifests/) that live outside the package under test.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
}
