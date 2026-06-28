package dsl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

// TemplateData is the context used during template expansion, for both step.Run
// expansion before step execution and output-capture template evaluation.
type TemplateData struct {
	Params  map[string]string
	Steps   map[string]StepData
	Stdout  string            // only set during output-capture template evaluation
	Secrets map[string]string // resolved secret values (must not be written to logs)
	Foreach map[string]string // set during foreach step execution; key = ForeachDef.Key
}

// StepData holds the captured outputs of a previously executed step.
type StepData struct {
	Outputs map[string]string
}

// secretsRefRe matches "{{ secrets.NAME }}" (without leading dot).
// dotSecretsRefRe matches "{{ .Secrets.NAME }}" (with leading dot).
// Both support names with hyphens. Names with hyphens are rewritten to
// {{ index .Secrets "NAME" }} because Go template dot-notation cannot
// access map keys that contain hyphens (it parses the hyphen as subtraction).
// Underscore-only names are left as-is by dotSecretsRefRe since .Secrets.NAME works fine.
var (
	secretsRefRe    = regexp.MustCompile(`\{\{(-?\s*)secrets\.([A-Za-z_][A-Za-z0-9_-]*)(\s*-?)\}\}`)
	dotSecretsRefRe = regexp.MustCompile(`\{\{(-?\s*)\.Secrets\.([A-Za-z_][A-Za-z0-9_-]*-[A-Za-z0-9_-]*)(\s*-?)\}\}`)
)

func rewriteToIndex(re *regexp.Regexp, tpl string) string {
	return re.ReplaceAllStringFunc(tpl, func(match string) string {
		sub := re.FindStringSubmatch(match)
		if len(sub) < 4 {
			return match
		}
		open := "{{" + sub[1]
		name := sub[2]
		close := sub[3] + "}}"
		return open + `index .Secrets "` + name + `"` + close
	})
}

// normalizeSecretsRefs rewrites secret references to use index notation,
// supporting both "{{ secrets.NAME }}" and "{{ .Secrets.NAME }}" forms.
// Names with hyphens (e.g. gitlab-token) are always rewritten; plain
// underscore names in the dot form are left untouched.
func normalizeSecretsRefs(tpl string) string {
	tpl = rewriteToIndex(secretsRefRe, tpl)
	tpl = rewriteToIndex(dotSecretsRefRe, tpl)
	return tpl
}

var funcMap = template.FuncMap{
	// grep returns the first line containing the pattern. Returns an empty string if there is no match.
	"grep": func(pattern, text string) string {
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, pattern) {
				return line
			}
		}
		return ""
	},
	// cut splits text by sep and returns the nth field (1-indexed, like cut -f).
	// Go template pipe: {{ .Stdout | cut "=" 2 }} → cut("=", 2, .Stdout)
	"cut": func(sep string, n int, text string) string {
		parts := strings.Split(text, sep)
		if n < 1 || n > len(parts) {
			return ""
		}
		return strings.TrimSpace(parts[n-1])
	},
	"trim": strings.TrimSpace,
	// split divides s by sep and returns a []string. Enables foreach source expressions
	// such as: {{ .Params.envs | split "," }}
	"split": func(sep, s string) []string {
		return strings.Split(s, sep)
	},
	// concat merges multiple []string slices into one. Enables combining dynamic
	// param lists with static fallbacks: {{ concat .Params.envs (list "fallback") }}
	"concat": func(lists ...[]string) []string {
		var out []string
		for _, l := range lists {
			out = append(out, l...)
		}
		return out
	},
	// list constructs a []string literal inline within a template expression.
	"list": func(items ...string) []string { return items },
	// hashFile returns the SHA-256 hex digest of all files matching glob (sorted).
	// Returns "" if no files match. Callers should ensure the glob matches at least one file
	// to avoid cache key collisions (e.g., use a literal suffix: "deps-{{ hashFile \"go.sum\" }}-v1").
	"hashFile": func(glob string) (string, error) {
		files, err := filepath.Glob(glob)
		if err != nil {
			return "", fmt.Errorf("hashFile glob: %w", err)
		}
		if len(files) == 0 {
			return "", nil
		}
		sort.Strings(files)
		h := sha256.New()
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return "", fmt.Errorf("hashFile read %q: %w", f, err)
			}
			fmt.Fprintf(h, "%s\x00", f)
			h.Write(data)
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	},
}

// ExpandTemplate evaluates tpl with Go text/template using parameter, step output,
// and stdout contexts.
// Returns an empty string when a map key is not found (missingkey=zero).
// References of the form "{{ secrets.NAME }}" are automatically converted to "{{ index .Secrets "NAME" }}", supporting hyphenated names.
func ExpandTemplate(tpl string, data TemplateData) (string, error) {
	tpl = normalizeSecretsRefs(tpl)
	t, err := template.New("").Funcs(funcMap).Option("missingkey=zero").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// ExpandAgentSelector applies template expansion with Job input parameters to each element of agentSelector.
// This allows the agentSelector to be determined dynamically at Run creation time,
// e.g. `pool:{{ .Params.pool }}`.
func ExpandAgentSelector(selector []string, params map[string]string) ([]string, error) {
	if len(selector) == 0 {
		return selector, nil
	}
	expanded := make([]string, len(selector))
	for i, s := range selector {
		v, err := ExpandTemplate(s, TemplateData{Params: params})
		if err != nil {
			return nil, fmt.Errorf("agentSelector[%d]: %w", i, err)
		}
		expanded[i] = v
	}
	return expanded, nil
}

// ExpandConcurrency expands {{ .Params.xxx }} templates in Concurrency.Mutex,
// each Semaphore.Pool, and each OrLock.Candidates entry using the Run's
// parameter values. OrLock.Name is never expanded (it is a fixed YAML value
// used to build the synthesized "{NAME}_LOCK_VALUE" parameter key). Returns a
// new Concurrency with expanded fields; the input is not mutated.
func ExpandConcurrency(c *Concurrency, params map[string]string) (*Concurrency, error) {
	if c == nil {
		return nil, nil
	}
	out := *c
	if c.Mutex != "" {
		v, err := ExpandTemplate(c.Mutex, TemplateData{Params: params})
		if err != nil {
			return nil, fmt.Errorf("concurrency.mutex: %w", err)
		}
		out.Mutex = v
	}
	if len(c.Semaphores) > 0 {
		out.Semaphores = make([]Semaphore, len(c.Semaphores))
		for i, nl := range c.Semaphores {
			v, err := ExpandTemplate(nl.Pool, TemplateData{Params: params})
			if err != nil {
				return nil, fmt.Errorf("concurrency.semaphores[%d].pool: %w", i, err)
			}
			out.Semaphores[i] = Semaphore{Pool: v, Capacity: nl.Capacity}
		}
	}
	if len(c.OrLocks) > 0 {
		out.OrLocks = make([]OrLock, len(c.OrLocks))
		for i, ol := range c.OrLocks {
			candidates, err := EvalForeachSource(ol.In, TemplateData{Params: params})
			if err != nil {
				return nil, fmt.Errorf("concurrency.orLocks[%d].in: %w", i, err)
			}
			out.OrLocks[i] = OrLock{Name: ol.Name, In: ForeachSource{Literal: candidates}}
		}
	}
	return &out, nil
}

// WebhookTemplateData is the template context used when evaluating webhook payload mappings and filters.
type WebhookTemplateData struct {
	Payload map[string]any
}

// ExpandWebhookTemplate expands a paramsMapping/filter template against a webhook payload.
func ExpandWebhookTemplate(tpl string, data WebhookTemplateData) (string, error) {
	t, err := template.New("").Funcs(funcMap).Option("missingkey=zero").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("parse webhook template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute webhook template: %w", err)
	}
	return buf.String(), nil
}
