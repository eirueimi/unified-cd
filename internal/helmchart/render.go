// Package helmchart renders the charts/unified-cd Helm chart with `helm
// template` so tests can assert on the resulting Kubernetes objects without a
// cluster.
package helmchart

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// ChartDir returns the absolute path of charts/unified-cd, resolved from this
// source file.
func ChartDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "charts", "unified-cd")
}

// Available reports whether the helm binary is on PATH.
func Available() bool {
	_, err := exec.LookPath("helm")
	return err == nil
}

// Object is one rendered Kubernetes object.
type Object map[string]any

// Kind returns the object's kind.
func (o Object) Kind() string {
	s, _ := o["kind"].(string)
	return s
}

// Name returns metadata.name.
func (o Object) Name() string { return o.metaString("name") }

// Namespace returns metadata.namespace.
func (o Object) Namespace() string { return o.metaString("namespace") }

func (o Object) metaString(key string) string {
	meta, _ := o["metadata"].(map[string]any)
	s, _ := meta[key].(string)
	return s
}

func args(namespace, valuesFile string, set []string) []string {
	a := []string{"template", "unified-cd", ChartDir(), "--namespace", namespace}
	if valuesFile != "" {
		a = append(a, "-f", valuesFile)
	}
	for _, s := range set {
		a = append(a, "--set", s)
	}
	return a
}

// RenderErr runs `helm template` and returns its stdout and stderr. It is meant
// for tests that expect a render failure; err is non-nil when helm exits
// non-zero.
func RenderErr(namespace string, set ...string) (stdout string, stderr string, err error) {
	return renderErr(namespace, "", set)
}

func renderErr(namespace, valuesFile string, set []string) (stdout string, stderr string, err error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("helm", args(namespace, valuesFile, set)...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err = cmd.Run()
	return out.String(), errb.String(), err
}

// Render renders the chart and returns every non-empty document. The test is
// skipped when helm is not installed and fails when helm exits non-zero.
func Render(t testing.TB, namespace string, set ...string) []Object {
	t.Helper()
	return RenderFile(t, namespace, "", set...)
}

// RenderFile is Render with a values file passed as `-f valuesFile` (an empty
// valuesFile renders the chart defaults). Any set values are applied on top of
// the file.
func RenderFile(t testing.TB, namespace, valuesFile string, set ...string) []Object {
	t.Helper()
	if !Available() {
		t.Skip("helm not on PATH")
	}
	stdout, stderr, err := renderErr(namespace, valuesFile, set)
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, stderr)
	}
	objs, err := parse(stdout)
	if err != nil {
		t.Fatalf("parse helm output: %v", err)
	}
	return objs
}

func parse(manifest string) ([]Object, error) {
	dec := yaml.NewDecoder(bytes.NewBufferString(manifest))
	var objs []Object
	for {
		// Decode into a plain map: yaml.v3 would otherwise decode nested maps as
		// Object too, which breaks type assertions on map[string]any.
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return objs, nil
		}
		if err != nil {
			return nil, err
		}
		if len(m) == 0 {
			continue
		}
		objs = append(objs, Object(m))
	}
}

// Find returns the first object with the given kind and name, or nil.
func Find(objs []Object, kind, name string) Object {
	for _, o := range objs {
		if o.Kind() == kind && o.Name() == name {
			return o
		}
	}
	return nil
}
