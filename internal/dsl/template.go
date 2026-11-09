package dsl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	Vars    map[string]string // plain-text variables; NOT secrets
	Steps   map[string]StepData
	Stdout  string            // only set during output-capture template evaluation
	Secrets map[string]string // resolved secret values (must not be written to logs)
	Foreach map[string]string // set during foreach step execution; key = ForeachDef.Key
	Matrix  map[string]string // set during matrix/foreach step execution; key = dimension name
}

// StepData holds the captured outputs of a previously executed step.
// Non-matrix steps store plain strings; matrix steps store an aggregated
// map[string]string keyed by combination key (e.g. "linux/amd64").
type StepData struct {
	Outputs map[string]any
	// ChildRunID is set for completed call: steps only. A plain string for
	// non-matrix call steps; a map[string]string keyed by combination key
	// for matrix call steps (mirroring how Outputs aggregates).
	ChildRunID any
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

// errHashFileUnavailable is returned by hashFile in every context except
// agent-side step-time expansion (cache key/path/restoreKeys, step text).
var errHashFileUnavailable = errors.New("hashFile is only available in step-time templates (cache key/path)")

// funcMap is the default, controller-safe function map. It performs no
// filesystem access, so new callers of ExpandTemplate are safe by default.
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
	// hashFile is unavailable in the default (controller-safe) function map: the
	// controller must never read its own filesystem on behalf of a job
	// definition. Step-time expansion on the agent uses ExpandStepTemplate,
	// which swaps in the real implementation (hashFileLocal).
	"hashFile": func(string) (string, error) { return "", errHashFileUnavailable },
	// keys returns the sorted keys of a map (matrix aggregated outputs helper).
	"keys": func(m any) []string {
		var out []string
		switch t := m.(type) {
		case map[string]string:
			for k := range t {
				out = append(out, k)
			}
		case map[string]any:
			for k := range t {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	},
	// values returns map values ordered by sorted key.
	"values": func(m any) []string {
		var ks []string
		switch t := m.(type) {
		case map[string]string:
			for k := range t {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			out := make([]string, len(ks))
			for i, k := range ks {
				out[i] = t[k]
			}
			return out
		case map[string]any:
			for k := range t {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			out := make([]string, len(ks))
			for i, k := range ks {
				out[i] = OutputValueString(t[k])
			}
			return out
		}
		return nil
	},
}

// ExpandTemplate evaluates tpl with Go text/template using parameter, step output,
// and stdout contexts. It is controller-safe: hashFile returns an error and no
// file is touched (see ExpandStepTemplate for the agent-side variant).
// Returns an empty string when a map key is not found (missingkey=zero).
// References of the form "{{ secrets.NAME }}" are automatically converted to "{{ index .Secrets "NAME" }}", supporting hyphenated names.
func ExpandTemplate(tpl string, data TemplateData) (string, error) {
	return expandWith(funcMap, tpl, data)
}

func expandWith(funcs template.FuncMap, tpl string, data TemplateData) (string, error) {
	tpl = normalizeSecretsRefs(tpl)
	t, err := template.New("").Funcs(funcs).Option("missingkey=zero").Parse(tpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// stepFuncMap returns funcMap with hashFile bound to the real, filesystem-reading
// implementation. Only agent-side step-time expansion may use it.
func stepFuncMap() template.FuncMap {
	m := make(template.FuncMap, len(funcMap))
	for k, v := range funcMap {
		m[k] = v
	}
	m["hashFile"] = hashFileLocal
	return m
}

// hashFileLocal returns the SHA-256 hex digest of all files matching glob (sorted).
// Returns "" if no files match. Callers should ensure the glob matches at least one file
// to avoid cache key collisions (e.g., use a literal suffix: "deps-{{ hashFile \"go.sum\" }}-v1").
// The glob is resolved relative to the agent's working directory (the template
// engine has no workspace root); absolute patterns and ".." segments are
// rejected, and only regular files are read (no devices, FIFOs or directories).
func hashFileLocal(glob string) (string, error) {
	if filepath.IsAbs(glob) || strings.HasPrefix(glob, "/") || strings.HasPrefix(glob, "\\") || filepath.VolumeName(glob) != "" {
		return "", fmt.Errorf("hashFile %q: absolute paths are not allowed", glob)
	}
	for _, seg := range strings.FieldsFunc(glob, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return "", fmt.Errorf("hashFile %q: \"..\" is not allowed", glob)
		}
	}
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
		fi, err := os.Lstat(f)
		if err != nil {
			return "", fmt.Errorf("hashFile stat %q: %w", f, err)
		}
		if !fi.Mode().IsRegular() {
			return "", fmt.Errorf("hashFile %q: not a regular file", f)
		}
		data, err := os.ReadFile(f)
		if err != nil {
			return "", fmt.Errorf("hashFile read %q: %w", f, err)
		}
		fmt.Fprintf(h, "%s\x00", f)
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExpandStepTemplate is ExpandTemplate for agent-side, step-time expansion: it
// additionally provides hashFile, which reads files from the agent's working
// directory. It must never be called from the controller.
func ExpandStepTemplate(tpl string, data TemplateData) (string, error) {
	return expandWith(stepFuncMap(), tpl, data)
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

// ExpandVolumes expands {{ .Params.xxx }} templates in every candidate of each
// volume's Name using the Run's parameter values, so a job can pick a volume by
// parameter (e.g. `unity-{{ .Params.LIB }}`). As and MountPath are never
// expanded. It returns a new slice with new Name slices; the input is not
// mutated. A failure names the entry and the candidate, e.g.
// "volumes[1].name[0]".
func ExpandVolumes(vs []VolumeMount, params map[string]string) ([]VolumeMount, error) {
	if len(vs) == 0 {
		return vs, nil
	}
	out := make([]VolumeMount, len(vs))
	for i, v := range vs {
		out[i] = v
		out[i].Name = make(StringOrSlice, len(v.Name))
		for j, n := range v.Name {
			expanded, err := ExpandTemplate(n, TemplateData{Params: params})
			if err != nil {
				return nil, fmt.Errorf("volumes[%d].name[%d]: %w", i, j, err)
			}
			out[i].Name[j] = expanded
		}
	}
	return out, nil
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
