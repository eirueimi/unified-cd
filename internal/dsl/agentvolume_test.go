package dsl

import (
	"strings"
	"testing"
)

const agentVolumeBase = `apiVersion: unified-cd/v1
kind: AgentVolume
metadata:
  name: unity-cache
spec:
  mode: exclusive
  size: 200Gi
  storageClassName: fast-ssd
  agentSelector: [role:unity]
`

func TestParseAgentVolume_Valid(t *testing.T) {
	v, err := ParseAgentVolume(strings.NewReader(agentVolumeBase))
	if err != nil {
		t.Fatalf("ParseAgentVolume: %v", err)
	}
	if v.Metadata.Name != "unity-cache" || v.Spec.Mode != VolumeModeExclusive ||
		v.Spec.Size != "200Gi" || v.Spec.StorageClassName != "fast-ssd" ||
		len(v.Spec.AgentSelector) != 1 || v.Spec.AgentSelector[0] != "role:unity" {
		t.Fatalf("unexpected parse result: %+v", v)
	}
}

func TestParseAgentVolume_Rejects(t *testing.T) {
	doc := func(kind, name, spec string) string {
		return "apiVersion: unified-cd/v1\nkind: " + kind + "\nmetadata:\n  name: " + name + "\nspec:\n" + spec
	}
	cases := []struct {
		name, yaml, want string
	}{
		{"missing mode", doc("AgentVolume", "v", "  size: 1Gi\n"), "mode"},
		{"bad mode", doc("AgentVolume", "v", "  mode: rw\n  size: 1Gi\n"), "mode"},
		{"missing size", doc("AgentVolume", "v", "  mode: shared\n"), "size"},
		{"zero size", doc("AgentVolume", "v", "  mode: shared\n  size: \"0\"\n"), "size"},
		{"garbage size", doc("AgentVolume", "v", "  mode: shared\n  size: abc\n"), "size"},
		{"uppercase name", doc("AgentVolume", "Unity", "  mode: shared\n  size: 1Gi\n"), "metadata.name"},
		{"slash name", doc("AgentVolume", "a/b", "  mode: shared\n  size: 1Gi\n"), "metadata.name"},
		{"unknown field", doc("AgentVolume", "v", "  mode: shared\n  size: 1Gi\n  path: /x\n"), "path"},
		{"wrong kind", doc("Vars", "v", "  mode: shared\n  size: 1Gi\n"), "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseAgentVolume(strings.NewReader(c.yaml))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestAgentVolumeMatches(t *testing.T) {
	cases := []struct {
		sel, labels []string
		want        bool
	}{
		{nil, []string{"a"}, true},
		{[]string{"role:unity"}, []string{"role:unity", "os:windows"}, true},
		{[]string{"role:unity", "os:linux"}, []string{"role:unity"}, false},
		{[]string{"role:unity"}, nil, false},
	}
	for _, c := range cases {
		if got := AgentVolumeMatches(c.sel, c.labels); got != c.want {
			t.Errorf("AgentVolumeMatches(%v, %v) = %v, want %v", c.sel, c.labels, got, c.want)
		}
	}
}

func TestParseAgentVolume_ReclaimPolicy(t *testing.T) {
	doc := func(extra string) string {
		return "apiVersion: unified-cd/v1\nkind: AgentVolume\nmetadata:\n  name: v\nspec:\n  mode: shared\n  size: 1Gi\n" + extra
	}
	for _, ok := range []string{"", "  reclaimPolicy: Delete\n", "  reclaimPolicy: Retain\n"} {
		if _, err := ParseAgentVolume(strings.NewReader(doc(ok))); err != nil {
			t.Errorf("%q: unexpected error: %v", ok, err)
		}
	}
	for _, bad := range []string{"  reclaimPolicy: delete\n", "  reclaimPolicy: Purge\n"} {
		_, err := ParseAgentVolume(strings.NewReader(doc(bad)))
		if err == nil || !strings.Contains(err.Error(), "reclaimPolicy") {
			t.Errorf("%q: want reclaimPolicy error, got %v", bad, err)
		}
	}
	_, err := ParseAgentVolume(strings.NewReader(doc("  agentSelector: [\"\"]\n")))
	if err == nil || !strings.Contains(err.Error(), "agentSelector") {
		t.Errorf("empty selector entry: want agentSelector error, got %v", err)
	}
}
