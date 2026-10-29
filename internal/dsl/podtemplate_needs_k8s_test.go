package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPodTemplateNeedsKubernetes covers the routing predicate the controller
// uses to decide whether a podTemplate job must be pinned to a Kubernetes agent
// (by auto-appending the "kubernetes" label) or may run on any agent — including
// the host/standard agent, whose claim pod honors only the container fields in
// HostSupportedContainerFields.
//
// The rule: needs kubernetes IFF the podTemplate uses something the host claim
// pod cannot honor. workspace.mountPath is host-OK, so it must NOT force
// kubernetes.
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
			"workspace.mountPath is host-OK",
			tmpl(PodTemplate{
				Workspace: &WorkspaceConfig{
					MountPath: "/workspace",
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
		{
			"resources.requests forces kubernetes (host drops it)",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "500m", "memory": "1Gi"},
					}}),
			}}}),
			true,
		},
		{
			"resources.limits only is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits": map[string]any{"cpu": "1", "memory": "2Gi"},
					}}),
			}}}),
			false,
		},
		{
			"resources with both requests and limits forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits":   map[string]any{"cpu": "1"},
						"requests": map[string]any{"cpu": "500m"},
					}}),
			}}}),
			true,
		},
		{
			"empty requests map requests nothing and is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{"requests": map[string]any{}}}),
			}}}),
			false,
		},
		{
			"env without a value key forces kubernetes (host cannot resolve valueFrom)",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{
						"name": "TOKEN",
						"valueFrom": map[string]any{"secretKeyRef": map[string]any{
							"name": "api", "key": "token",
						}},
					}}}),
			}}}),
			true,
		},
		{
			"literal env value is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{"name": "MODE", "value": "fast"}}}),
			}}}),
			false,
		},
		{
			"empty-string env value is a literal the host honors",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{"name": "MODE", "value": ""}}}),
			}}}),
			false,
		},
		{
			"one valueFrom among literals still forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{
						map[string]any{"name": "MODE", "value": "fast"},
						map[string]any{"name": "TOKEN", "valueFrom": map[string]any{
							"fieldRef": map[string]any{"fieldPath": "metadata.name"},
						}},
					}}),
			}}}),
			true,
		},
		{
			"nameless env entry is skipped, matching the host builder",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{"valueFrom": map[string]any{
						"fieldRef": map[string]any{"fieldPath": "metadata.name"},
					}}}}),
			}}}),
			false,
		},
		{
			"resources.limits with an extended resource key forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits": map[string]any{"cpu": "1", "memory": "2Gi", "nvidia.com/gpu": "1"},
					}}),
			}}}),
			true,
		},
		{
			"resources.claims forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits": map[string]any{"cpu": "1", "memory": "2Gi"},
						"claims": []any{map[string]any{"name": "gpu-claim"}},
					}}),
			}}}),
			true,
		},
		{
			"a non-string cpu limit forces kubernetes (host's string assertion silently drops it)",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits": map[string]any{"cpu": 1, "memory": "2Gi"},
					}}),
			}}}),
			true,
		},
		{
			"a second container's requests forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim"}),
				container(map[string]any{"name": "cache", "image": "redis:7",
					"resources": map[string]any{
						"requests": map[string]any{"memory": "256Mi"},
					}}),
			}}}),
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PodTemplateNeedsKubernetes(tc.pt))
		})
	}
}

// TestPodTemplateNeedsKubernetes_SecurityContext covers design section 4: a
// securityContext no longer pins a job on its own. Only a field or value the
// host does not honour does, and a mixture pins.
func TestPodTemplateNeedsKubernetes_SecurityContext(t *testing.T) {
	withPod := func(sc any) *PodTemplate {
		return &PodTemplate{Spec: map[string]any{
			"securityContext": sc,
			"containers":      []any{map[string]any{"name": "job", "image": "x"}},
		}}
	}
	withCtr := func(sc any) *PodTemplate {
		return &PodTemplate{Spec: map[string]any{"containers": []any{
			map[string]any{"name": "job", "image": "x", "securityContext": sc},
		}}}
	}

	cases := []struct {
		name string
		pt   *PodTemplate
		want bool
	}{
		{"pod identity and fsGroup are honoured", withPod(map[string]any{
			"runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "runAsNonRoot": true,
			"supplementalGroups": []any{2000}, "fsGroupChangePolicy": "OnRootMismatch",
		}), false},
		{"pod float64 ids (store round trip) are honoured", withPod(map[string]any{"runAsUser": float64(1000), "fsGroup": float64(1000)}), false},
		{"pod supplementalGroupsPolicy Merge is honoured", withPod(map[string]any{"supplementalGroupsPolicy": "Merge"}), false},
		{"pod supplementalGroupsPolicy Strict pins", withPod(map[string]any{"supplementalGroupsPolicy": "Strict"}), true},
		{"pod sysctls pins", withPod(map[string]any{"sysctls": []any{}}), true},
		{"pod seLinuxOptions pins", withPod(map[string]any{"seLinuxOptions": map[string]any{"level": "s0"}}), true},
		{"pod unknown key pins", withPod(map[string]any{"fooBar": true}), true},
		{"pod seccomp Unconfined pins", withPod(map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}}), true},
		{"pod seccomp RuntimeDefault is honoured", withPod(map[string]any{"seccompProfile": map[string]any{"type": "RuntimeDefault"}}), false},
		{"container hardening fields are honoured", withCtr(map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}}}), false},
		{"container seccomp Unconfined pins", withCtr(map[string]any{"seccompProfile": map[string]any{"type": "Unconfined"}}), true},
		{"pod securityContext that is not a map pins", withPod("nope"), true},
		{"pod securityContext null is unset", withPod(nil), false},
		{"pod identity plus a pinned value pins", withPod(map[string]any{"runAsUser": 1000, "sysctls": []any{}}), true},
		{"container identity is honoured", withCtr(map[string]any{"runAsUser": 1000, "runAsGroup": 1000, "runAsNonRoot": true}), false},
		{"container privileged true pins", withCtr(map[string]any{"runAsUser": 1000, "privileged": true}), true},
		{"container capabilities add SYS_ADMIN pins", withCtr(map[string]any{"capabilities": map[string]any{"add": []any{"SYS_ADMIN"}}}), true},
		{"container unknown key pins", withCtr(map[string]any{"fooBar": 1}), true},
		{"container securityContext that is not a map pins", withCtr([]any{}), true},
		{"container seLinuxOptions pins", withCtr(map[string]any{"seLinuxOptions": map[string]any{}}), true},
		{"container identity but pod pinned value pins", &PodTemplate{Spec: map[string]any{
			"securityContext": map[string]any{"sysctls": []any{}},
			"containers": []any{map[string]any{"name": "job", "image": "x",
				"securityContext": map[string]any{"runAsUser": 1000}}},
		}}, true},
		{"honoured securityContext but another pinned container field pins", &PodTemplate{Spec: map[string]any{
			"securityContext": map[string]any{"fsGroup": 1000},
			"containers":      []any{map[string]any{"name": "job", "image": "x", "volumeMounts": []any{}}},
		}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PodTemplateNeedsKubernetes(tc.pt))
		})
	}
}
