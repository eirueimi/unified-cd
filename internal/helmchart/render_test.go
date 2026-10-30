package helmchart

import (
	"strings"
	"testing"
)

func TestRenderEmptyChartSucceeds(t *testing.T) {
	if !Available() {
		t.Skip("helm not on PATH")
	}
	_, stderr, err := RenderErr("unified-cd")
	if err != nil {
		t.Fatalf("render with defaults failed: %v\n%s", err, stderr)
	}
}

func TestValidateFailures(t *testing.T) {
	if !Available() {
		t.Skip("helm not on PATH")
	}
	tests := []struct {
		name string
		set  []string
		want string
	}{
		{
			name: "nothing enabled",
			set:  []string{"controller.enabled=false", "agent.enabled=false", "bundledStores.enabled=false"},
			want: "nothing to install: enable controller.enabled or agent.enabled",
		},
		{
			name: "bundled stores without controller",
			set:  []string{"controller.enabled=false", "agent.server=https://cd.example.com"},
			want: "bundledStores.enabled requires controller.enabled",
		},
		{
			name: "agent server required without controller",
			set:  []string{"controller.enabled=false", "bundledStores.enabled=false"},
			want: "agent.server is required when controller.enabled is false",
		},
		{
			name: "agent server not https",
			set:  []string{"agent.server=http://cd.example.com"},
			want: "agent.server must be an https:// URL",
		},
		{
			name: "ingress with trial credentials",
			set:  []string{"ingress.enabled=true"},
			want: "ingress.enabled with the bundled trial credentials would expose dev-token-change-me: set controller.existingSecret to a Secret you created, or disable ingress",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := RenderErr("unified-cd", tc.set...)
			if err == nil {
				t.Fatalf("expected render failure, got success")
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr missing %q:\n%s", tc.want, stderr)
			}
		})
	}
}

func TestFind(t *testing.T) {
	objs := []Object{
		{"kind": "ConfigMap", "metadata": map[string]any{"name": "a", "namespace": "ns"}},
	}
	got := Find(objs, "ConfigMap", "a")
	if got == nil || got.Namespace() != "ns" || got.Kind() != "ConfigMap" || got.Name() != "a" {
		t.Fatalf("Find returned %v", got)
	}
	if Find(objs, "Secret", "a") != nil {
		t.Fatal("expected nil for absent object")
	}
}
