package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPodTemplateNeedsKubernetes covers the routing predicate the controller
// uses to decide whether a podTemplate job must be pinned to a Kubernetes agent
// (by auto-appending the "kubernetes" label) or may run on any agent — including
// the host/standard agent, whose claim pod honors only the container fields in
// HostSupportedContainerFields and degrades workspace.pvc to a bind mount.
//
// The rule: needs kubernetes IFF the podTemplate uses something the host claim
// pod cannot honor. workspace.pvc / mountPath are host-OK (bind-mount degrade),
// so they must NOT force kubernetes.
func TestPodTemplateNeedsKubernetes(t *testing.T) {
	container := func(fields map[string]any) map[string]any { return fields }
	tmpl := func(pt PodTemplate) *PodTemplate { return &pt }

	cases := []struct {
		name string
		pt   *PodTemplate
		want bool
	}{
		{"nil", nil, false},
		{
			"plain name+image containers",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim"}),
				container(map[string]any{"name": "ruff", "image": "python:3.12-slim"}),
			}}}),
			false,
		},
		{
			"workspace.pvc is host-OK (bind-mount degrade)",
			tmpl(PodTemplate{
				Workspace: &WorkspaceConfig{
					MountPath: "/workspace",
					PVC:       &WorkspacePVC{StorageClassName: "standard", StorageRequest: "5Gi", AccessMode: "ReadWriteOnce"},
				},
				Spec: map[string]any{"containers": []any{
					container(map[string]any{"name": "job", "image": "python:3.12-slim"}),
				}},
			}),
			false,
		},
		{
			"container env + resources are host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "x", "env": []any{}, "resources": map[string]any{}}),
			}}}),
			false,
		},
		{
			"reuse is a perf hint, host-OK",
			tmpl(PodTemplate{Reuse: true, Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "x"}),
			}}}),
			false,
		},
		{
			"container command is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "x", "command": []any{"sleep", "1"}}),
			}}}),
			false,
		},
		{
			"container args is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "x", "args": []any{"--foo"}}),
			}}}),
			false,
		},
		{
			"container volumeMounts is host-unsupported",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "x", "volumeMounts": []any{}}),
			}}}),
			true,
		},
		{
			"named agent-side template requires k8s",
			tmpl(PodTemplate{Name: "golang", Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "x"}),
			}}}),
			true,
		},
		{
			"override requires k8s (host reads only spec.containers)",
			tmpl(PodTemplate{
				Override: &PodSpecPatch{Containers: []map[string]any{{"name": "trivy", "image": "aquasec/trivy"}}},
				Spec:     map[string]any{"containers": []any{container(map[string]any{"name": "job", "image": "x"})}},
			}),
			true,
		},
		{
			"pod-level spec key beyond containers requires k8s",
			tmpl(PodTemplate{Spec: map[string]any{
				"containers":   []any{container(map[string]any{"name": "job", "image": "x"})},
				"nodeSelector": map[string]any{"disktype": "ssd"},
			}}),
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PodTemplateNeedsKubernetes(tc.pt))
		})
	}
}
