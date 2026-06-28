package gittemplate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/unified-cd/unified-cd/internal/dsl"
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

	resolvedSteps, err := r.resolveSteps(ctx, spec.Steps, credFn, 0, nil)
	if err != nil {
		return nil, err
	}
	spec.Steps = resolvedSteps

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
) ([]dsl.StepEntry, error) {
	if depth > maxUsesDepth {
		return nil, newResolveError("uses nesting exceeds max depth %d", maxUsesDepth)
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
	for _, s := range steps {
		if s.Uses == nil {
			out = append(out, s)
			continue
		}
		rawURI := s.Uses.Job

		for _, p := range path {
			if p == rawURI {
				return nil, newResolveError("circular uses reference: %q", rawURI)
			}
		}

		uri, err := ParseURI(rawURI)
		if err != nil {
			return nil, wrapResolveError(fmt.Errorf("step %q: parse git URI: %w", s.Name, err))
		}

		cred, err := credFn(ctx, uri.Host)
		if err != nil {
			return nil, fmt.Errorf("step %q: get credential for %q: %w", s.Name, uri.Host, err)
		}
		rawYAML, err := r.fetch(ctx, uri, cred)
		if err != nil {
			return nil, fmt.Errorf("step %q: fetch %q: %w", s.Name, rawURI, err)
		}

		var job dsl.Job
		if err := yaml.Unmarshal(rawYAML, &job); err != nil {
			return nil, newResolveError("step %q: parse fetched YAML from %q: %v", s.Name, rawURI, err)
		}
		if err := job.Validate(); err != nil {
			return nil, newResolveError("step %q: fetched job %q failed validation: %v", s.Name, rawURI, err)
		}

		nestedPath := append(append([]string{}, path...), rawURI)
		nestedSteps, err := r.resolveSteps(ctx, job.Spec.Steps, credFn, depth+1, nestedPath)
		if err != nil {
			return nil, err
		}
		job.Spec.Steps = nestedSteps

		expanded, err := expandUsesStep(s.Name, s.Uses.WithAsStrings(), job.Spec)
		if err != nil {
			return nil, newResolveError("step %q: expand uses: %v", s.Name, err)
		}

		for _, es := range expanded {
			if es.Name == s.Name {
				continue // expected: the output-capture step intentionally reuses the uses step's own name
			}
			if seen[es.Name] {
				return nil, newResolveError("step %q: expanded step name %q collides with an existing step", s.Name, es.Name)
			}
			seen[es.Name] = true
		}

		out = append(out, expanded...)
	}
	return out, nil
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
