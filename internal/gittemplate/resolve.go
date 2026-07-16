package gittemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// maxUsesDepth bounds recursive uses resolution (a template referencing another
// template, etc.) to prevent unbounded expansion.
const maxUsesDepth = 10

// Credential holds plaintext auth for a git host.
type Credential struct {
	Token  string // HTTPS token
	SSHKey string // SSH private key content
}

// CredentialFunc returns credentials for the given host.
// Return zero Credential for public repos (no error).
type CredentialFunc func(ctx context.Context, host string) (Credential, error)

// FetcherInterface abstracts git fetch for testing.
type FetcherInterface interface {
	Fetch(ctx context.Context, uri URI, token, sshKey string) ([]byte, error)
}

// Resolver resolves git:// URIs to their YAML content, with optional caching.
type Resolver struct {
	Fetcher FetcherInterface
	Cache   *Cache // nil = no caching
}

// NewResolver creates a Resolver. cache may be nil to disable caching.
func NewResolver(fetcher FetcherInterface, cache *Cache) *Resolver {
	return &Resolver{Fetcher: fetcher, Cache: cache}
}

// HasGitURIs reports whether specJSON contains any unresolved `uses` step
// referencing a git:// URI. The byte-substring check is a fast path only
// ("definitely nothing to do" when it misses); the actual answer comes from
// inspecting step.Uses structurally, so a fully-resolved run whose other
// fields happen to contain the literal "git://" (e.g. an env value cloning a
// git:// remote) isn't mistaken for one that still needs resolution.
func HasGitURIs(specJSON []byte) bool {
	if !bytes.Contains(specJSON, []byte(`"git://`)) {
		return false
	}
	var spec dsl.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return false
	}
	for _, s := range spec.Steps {
		if s.Uses != nil && strings.HasPrefix(s.Uses.Job, "git://") {
			return true
		}
	}
	for _, s := range spec.Finally {
		if s.Uses != nil && strings.HasPrefix(s.Uses.Job, "git://") {
			return true
		}
	}
	return false
}

// ResolveSpec recursively expands every `uses` step whose Job is a git:// URI,
// inlining the referenced job's steps directly into specJSON's step list (see
// expandUsesStep). Returns specJSON unchanged if no git:// URIs are found.
//
// Returns a deterministic error (see IsResolveError) for malformed YAML, circular
// uses references, exceeding maxUsesDepth, or a post-expansion step name collision.
// Any other error (fetch/network/credential lookup) is transient and safe to retry.
func (r *Resolver) ResolveSpec(
	ctx context.Context,
	specJSON []byte,
	credFn CredentialFunc,
) ([]byte, error) {
	if !HasGitURIs(specJSON) {
		return specJSON, nil
	}

	var spec dsl.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, wrapResolveError(fmt.Errorf("unmarshal spec: %w", err))
	}

	resolvedSteps, contrib, err := r.resolveSteps(ctx, spec.Steps, credFn, 0, nil)
	if err != nil {
		return nil, err
	}
	spec.Steps = resolvedSteps

	if len(spec.Finally) > 0 {
		resolvedFinally, fcontrib, ferr := r.resolveSteps(ctx, spec.Finally, credFn, 0, nil)
		if ferr != nil {
			return nil, ferr
		}
		spec.Finally = resolvedFinally
		contrib.containers = append(contrib.containers, fcontrib.containers...)
		contrib.volumes = append(contrib.volumes, fcontrib.volumes...)
		contrib.finally = append(contrib.finally, fcontrib.finally...)
	}

	if err := mergeContribution(&spec, contrib); err != nil {
		return nil, err
	}

	// Splice every uses: template's own finally: steps into the caller's
	// finally phase, after the caller's own (already-resolved) finally
	// entries — contrib.finally was built in encounter order across both the
	// main-steps and finally-steps resolutions above, so a uses: step's
	// template-finally lands after whatever the caller wrote by hand, in the
	// order the uses: steps appear in the spec.
	spec.Finally = append(spec.Finally, contrib.finally...)

	// Step names must be unique across the whole resolved spec (parse enforces
	// this for authored names via a shared nameSet; expansion must not
	// reintroduce a duplicate across the Steps/Finally boundary).
	if err := checkGlobalNameCollisions(spec.Steps, spec.Finally); err != nil {
		return nil, err
	}

	out, err := json.Marshal(spec)
	if err != nil {
		return nil, wrapResolveError(fmt.Errorf("marshal resolved spec: %w", err))
	}
	return out, nil
}

// resolveSteps walks steps, expanding any `uses` step that references a git:// URI.
// path tracks the chain of URIs currently being resolved, for cycle detection.
func (r *Resolver) resolveSteps(
	ctx context.Context,
	steps []dsl.StepEntry,
	credFn CredentialFunc,
	depth int,
	path []string,
) ([]dsl.StepEntry, podContribution, error) {
	if depth > maxUsesDepth {
		return nil, podContribution{}, newResolveError("uses nesting exceeds max depth %d", maxUsesDepth)
	}

	seen := make(map[string]bool, len(steps))
	for _, s := range steps {
		if s.Name != "" {
			seen[s.Name] = true
		}
		for _, p := range s.Parallel {
			if p.Name != "" {
				seen[p.Name] = true
			}
		}
	}

	var out []dsl.StepEntry
	var contrib podContribution
	for _, s := range steps {
		if s.Uses == nil {
			out = append(out, s)
			continue
		}
		rawURI := s.Uses.Job

		for _, p := range path {
			if p == rawURI {
				return nil, podContribution{}, newResolveError("circular uses reference: %q", rawURI)
			}
		}

		uri, err := ParseURI(rawURI)
		if err != nil {
			return nil, podContribution{}, wrapResolveError(fmt.Errorf("step %q: parse git URI: %w", s.Name, err))
		}

		cred, err := credFn(ctx, uri.Host)
		if err != nil {
			return nil, podContribution{}, fmt.Errorf("step %q: get credential for %q: %w", s.Name, uri.Host, err)
		}
		rawYAML, err := r.fetch(ctx, uri, cred)
		if err != nil {
			return nil, podContribution{}, fmt.Errorf("step %q: fetch %q: %w", s.Name, rawURI, err)
		}

		tpl, err := dsl.ParseJobTemplate(rawYAML)
		if err != nil {
			return nil, podContribution{}, newResolveError("step %q: fetched template %q: %v", s.Name, rawURI, err)
		}
		tplSpec := tpl.ToSpec()

		nestedPath := append(append([]string{}, path...), rawURI)
		nestedSteps, nestedContrib, err := r.resolveSteps(ctx, tplSpec.Steps, credFn, depth+1, nestedPath)
		if err != nil {
			return nil, podContribution{}, err
		}
		tplSpec.Steps = nestedSteps

		// A scope-mode uses (runsIn.image) must contribute nothing to the
		// caller's podTemplate — the template runs in its own scope pod. That
		// invariant is enforced directly against tplSpec.PodTemplate inside
		// expandUsesStep below, but a nested uses: further down inside this
		// template (itself resolved in non-scope mode, so free to declare its
		// own podTemplate) can still bubble a contribution up through
		// nestedContrib. Reject that here before it reaches expandUsesStep,
		// which would otherwise merge it into the caller's pod.
		scopeMode := s.RunsIn != nil && s.RunsIn.Image != ""
		if scopeMode && (len(nestedContrib.containers) > 0 || len(nestedContrib.volumes) > 0) {
			return nil, podContribution{}, newResolveError("step %q: a template used with runsIn.image (scope mode) cannot contribute pod containers/volumes to the caller (nested uses template declares a podTemplate)", s.Name)
		}

		expanded, expandContrib, err := expandUsesStep(s.Name, s.Uses.WithAsStrings(), tplSpec, s.RunsIn, s.Container, s.If)
		if err != nil {
			return nil, podContribution{}, newResolveError("step %q: expand uses: %v", s.Name, err)
		}

		for _, es := range expanded {
			if es.Name == s.Name {
				continue // expected: the output-capture step intentionally reuses the uses step's own name
			}
			if seen[es.Name] {
				return nil, podContribution{}, newResolveError("step %q: expanded step name %q collides with an existing step", s.Name, es.Name)
			}
			seen[es.Name] = true
		}

		out = append(out, expanded...)
		contrib.containers = append(contrib.containers, nestedContrib.containers...)
		contrib.containers = append(contrib.containers, expandContrib.containers...)
		contrib.volumes = append(contrib.volumes, nestedContrib.volumes...)
		contrib.volumes = append(contrib.volumes, expandContrib.volumes...)
		contrib.finally = append(contrib.finally, nestedContrib.finally...)
		contrib.finally = append(contrib.finally, expandContrib.finally...)
	}
	return out, contrib, nil
}

// mergeContribution fills spec.PodTemplate with the containers and volumes
// contributed by uses: templates that the caller lacks. A name already present
// (caller or a previously-merged contribution) is kept once if the definitions
// are JSON-equal, or is a deterministic resolve error if they differ. Reserved
// names were already rejected at contribution time.
func mergeContribution(spec *dsl.Spec, contrib podContribution) error {
	if len(contrib.containers) == 0 && len(contrib.volumes) == 0 {
		return nil
	}
	if spec.PodTemplate == nil {
		spec.PodTemplate = &dsl.PodTemplate{}
	}
	if spec.PodTemplate.Spec == nil {
		spec.PodTemplate.Spec = map[string]any{}
	}
	if err := mergeDefs(spec.PodTemplate, "containers", contrib.containers); err != nil {
		return err
	}
	return mergeDefs(spec.PodTemplate, "volumes", contrib.volumes)
}

// mergeDefs gap-fills named definition maps into pt.Spec[key].
func mergeDefs(pt *dsl.PodTemplate, key string, defs []map[string]any) error {
	if len(defs) == 0 {
		return nil
	}
	rawList, _ := pt.Spec[key].([]any)
	existing := map[string]map[string]any{}
	for _, r := range rawList {
		if d, ok := r.(map[string]any); ok {
			if n := dsl.DefName(d); n != "" {
				existing[n] = d
			}
		}
	}
	for _, d := range defs {
		name := dsl.DefName(d)
		if name == "" {
			continue
		}
		if prev, ok := existing[name]; ok {
			eq, err := jsonEqual(prev, d)
			if err != nil {
				return wrapResolveError(fmt.Errorf("compare %s %q: %w", strings.TrimSuffix(key, "s"), name, err))
			}
			if !eq {
				return newResolveError("%s %q is defined differently by the caller (or another uses template) and a uses template; rename one or align their definitions", strings.TrimSuffix(key, "s"), name)
			}
			continue // identical -> dedup
		}
		existing[name] = d
		rawList = append(rawList, d)
	}
	pt.Spec[key] = rawList
	return nil
}

// jsonEqual compares two values by their canonical JSON encoding (map keys are
// sorted by encoding/json), so it is order- and numeric-representation-stable
// across YAML- and JSON-sourced maps.
func jsonEqual(a, b any) (bool, error) {
	ba, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(ba, bb), nil
}

// checkGlobalNameCollisions rejects a resolved spec whose expanded step names
// collide across the main DAG and finally lists. Within each list resolveSteps'
// own seen-map already guards; this closes the cross-list hole opened by
// expanding uses in both lists independently.
func checkGlobalNameCollisions(steps, finally []dsl.StepEntry) error {
	seen := map[string]bool{}
	record := func(name string) error {
		if name == "" {
			return nil
		}
		if seen[name] {
			return newResolveError("step name %q appears in both the main steps and finally after uses expansion; rename one", name)
		}
		seen[name] = true
		return nil
	}
	for _, list := range [][]dsl.StepEntry{steps, finally} {
		for _, e := range list {
			if err := record(e.Name); err != nil {
				return err
			}
			for _, p := range e.Parallel {
				if err := record(p.Name); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *Resolver) fetch(ctx context.Context, uri URI, cred Credential) ([]byte, error) {
	if r.Cache != nil {
		if data, ok := r.Cache.Get(ctx, uri); ok {
			return data, nil
		}
	}
	data, err := r.Fetcher.Fetch(ctx, uri, cred.Token, cred.SSHKey)
	if err != nil {
		return nil, err
	}
	if r.Cache != nil {
		r.Cache.Put(ctx, uri, data)
	}
	return data, nil
}
