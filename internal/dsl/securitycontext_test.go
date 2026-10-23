package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func i64(n int64) *int64 { return &n }

func TestResolveSecurity_Precedence(t *testing.T) {
	cases := []struct {
		name   string
		pod    map[string]any
		ctr    map[string]any
		assert func(t *testing.T, e EffectiveSecurity)
	}{
		{"both nil is empty", nil, nil, func(t *testing.T, e EffectiveSecurity) {
			assert.Equal(t, EffectiveSecurity{}, e)
		}},
		{"pod runAsUser/runAsGroup apply", map[string]any{"runAsUser": 1000, "runAsGroup": 2000}, nil, func(t *testing.T, e EffectiveSecurity) {
			assert.Equal(t, i64(1000), e.UID)
			assert.Equal(t, i64(2000), e.GID)
		}},
		{"container overrides pod for UID and GID", map[string]any{"runAsUser": 1000, "runAsGroup": 2000},
			map[string]any{"runAsUser": 5, "runAsGroup": 6}, func(t *testing.T, e EffectiveSecurity) {
				assert.Equal(t, i64(5), e.UID)
				assert.Equal(t, i64(6), e.GID)
			}},
		{"container runAsUser 0 overrides a non-zero pod value", map[string]any{"runAsUser": 1000},
			map[string]any{"runAsUser": 0}, func(t *testing.T, e EffectiveSecurity) {
				assert.Equal(t, i64(0), e.UID)
			}},
		{"container sets only UID, GID falls back to pod", map[string]any{"runAsGroup": 2000},
			map[string]any{"runAsUser": 5}, func(t *testing.T, e EffectiveSecurity) {
				assert.Equal(t, i64(5), e.UID)
				assert.Equal(t, i64(2000), e.GID)
			}},
		{"runAsNonRoot: pod true", map[string]any{"runAsNonRoot": true}, nil, func(t *testing.T, e EffectiveSecurity) {
			assert.True(t, e.RunAsNonRoot)
		}},
		{"runAsNonRoot: container false overrides pod true", map[string]any{"runAsNonRoot": true},
			map[string]any{"runAsNonRoot": false}, func(t *testing.T, e EffectiveSecurity) {
				assert.False(t, e.RunAsNonRoot)
			}},
		{"runAsNonRoot: container true over pod unset", nil, map[string]any{"runAsNonRoot": true}, func(t *testing.T, e EffectiveSecurity) {
			assert.True(t, e.RunAsNonRoot)
		}},
		{"supplementalGroups, fsGroup, policy are pod-only", map[string]any{
			"supplementalGroups": []any{10, 20}, "fsGroup": 30, "fsGroupChangePolicy": "OnRootMismatch",
		}, nil, func(t *testing.T, e EffectiveSecurity) {
			assert.Equal(t, []int64{10, 20}, e.SupplementalGroups)
			assert.Equal(t, i64(30), e.FSGroup)
			assert.Equal(t, "OnRootMismatch", e.FSGroupChangePolicy)
		}},
		{"supplementalGroups and fsGroup on a container are ignored", nil, map[string]any{
			"supplementalGroups": []any{10}, "fsGroup": 30,
		}, func(t *testing.T, e EffectiveSecurity) {
			assert.Empty(t, e.SupplementalGroups)
			assert.Nil(t, e.FSGroup)
		}},
		{"hardening fields are container-only", map[string]any{"readOnlyRootFilesystem": true},
			map[string]any{
				"allowPrivilegeEscalation": false,
				"readOnlyRootFilesystem":   true,
				"capabilities":             map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}},
			}, func(t *testing.T, e EffectiveSecurity) {
				assert.True(t, e.NoNewPrivileges)
				assert.True(t, e.ReadOnlyRootfs)
				assert.Equal(t, []string{"ALL"}, e.CapDrop)
				assert.Equal(t, []string{"NET_BIND_SERVICE"}, e.CapAdd)
			}},
		{"allowPrivilegeEscalation true sets no flag", nil, map[string]any{"allowPrivilegeEscalation": true}, func(t *testing.T, e EffectiveSecurity) {
			assert.False(t, e.NoNewPrivileges)
		}},
		{"pod readOnlyRootFilesystem is not inherited", map[string]any{"readOnlyRootFilesystem": true}, nil, func(t *testing.T, e EffectiveSecurity) {
			assert.False(t, e.ReadOnlyRootfs)
		}},
		{"nil values count as unset", map[string]any{"runAsUser": 7}, map[string]any{"runAsUser": nil}, func(t *testing.T, e EffectiveSecurity) {
			assert.Equal(t, i64(7), e.UID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := ResolveSecurity(tc.pod, tc.ctr)
			require.NoError(t, err)
			tc.assert(t, e)
		})
	}
}

func TestResolveSecurity_IntegerDecoding(t *testing.T) {
	// YAML yields int, the JSON round trip yields float64: both decode the same.
	a, err := ResolveSecurity(map[string]any{"runAsUser": int(1000), "supplementalGroups": []any{int(5)}}, nil)
	require.NoError(t, err)
	b, err := ResolveSecurity(map[string]any{"runAsUser": float64(1000), "supplementalGroups": []any{float64(5)}}, nil)
	require.NoError(t, err)
	assert.Equal(t, a, b)
	c, err := ResolveSecurity(map[string]any{"runAsUser": int64(1000), "supplementalGroups": []any{int64(5)}}, nil)
	require.NoError(t, err)
	assert.Equal(t, a, c)

}

func TestResolveSecurity_Errors(t *testing.T) {
	cases := []struct {
		name string
		pod  map[string]any
		ctr  map[string]any
		want string
	}{
		{"fractional float", map[string]any{"runAsUser": 1000.5}, nil, "podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got 1000.5)"},
		{"string", map[string]any{"runAsUser": "1000"}, nil, `must be an integer between 0 and 2147483647 (got "1000")`},
		{"negative", nil, map[string]any{"runAsGroup": -1}, "container.securityContext.runAsGroup"},
		{"above int32", map[string]any{"fsGroup": 2147483648}, nil, "securityContext.fsGroup: must be an integer"},
		{"above int32 float", map[string]any{"fsGroup": float64(2147483648)}, nil, "securityContext.fsGroup: must be an integer"},
		{"bool", map[string]any{"runAsUser": true}, nil, "must be an integer"},
		{"bad group element", map[string]any{"supplementalGroups": []any{1, "x"}}, nil, "supplementalGroups[1]"},
		{"groups not a list", map[string]any{"supplementalGroups": 5}, nil, "supplementalGroups: must be a list of integers"},
		{"bad policy", map[string]any{"fsGroupChangePolicy": "Never"}, nil, `fsGroupChangePolicy: must be one of Always, OnRootMismatch (got "Never")`},
		{"runAsNonRoot not bool", map[string]any{"runAsNonRoot": "yes"}, nil, "runAsNonRoot: must be a boolean"},
		{"caps not a map", nil, map[string]any{"capabilities": []any{"ALL"}}, "capabilities: must be a map"},
		{"cap not a string", nil, map[string]any{"capabilities": map[string]any{"drop": []any{1}}}, "capabilities.drop[0]: must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveSecurity(tc.pod, tc.ctr)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// secRow is one row of the classification tables. For a tier B field, needsK8s
// is what the row yields once tier B is honoured; while hostHonoursHardeningFields
// is false the field is pinned, so the expectation flips to true. Turning tier B
// on therefore changes one constant, not the rows.
type secRow struct {
	name   string
	sc     map[string]any
	tierB  bool
	needsK bool
}

func (r secRow) want() bool {
	if r.tierB && !hostHonoursHardeningFields {
		return true
	}
	return r.needsK
}

func profile(typ string) map[string]any { return map[string]any{"type": typ} }

func TestPodSecurityNeedsKubernetes(t *testing.T) {
	rows := []secRow{
		{"empty", map[string]any{}, false, false},
		{"nil map", nil, false, false},
		{"runAsUser", map[string]any{"runAsUser": 1000}, false, false},
		{"runAsUser float64", map[string]any{"runAsUser": float64(1000)}, false, false},
		{"runAsUser wrong type is pinned", map[string]any{"runAsUser": "1000"}, false, true},
		{"runAsGroup", map[string]any{"runAsGroup": 1000}, false, false},
		{"runAsNonRoot", map[string]any{"runAsNonRoot": true}, false, false},
		{"supplementalGroups", map[string]any{"supplementalGroups": []any{1, 2}}, false, false},
		{"supplementalGroups bad element is pinned", map[string]any{"supplementalGroups": []any{"x"}}, false, true},
		{"fsGroup", map[string]any{"fsGroup": 2000}, false, false},
		{"fsGroupChangePolicy Always", map[string]any{"fsGroupChangePolicy": "Always"}, false, false},
		{"fsGroupChangePolicy OnRootMismatch", map[string]any{"fsGroupChangePolicy": "OnRootMismatch"}, false, false},
		{"fsGroupChangePolicy other", map[string]any{"fsGroupChangePolicy": "Never"}, false, true},
		{"supplementalGroupsPolicy Merge", map[string]any{"supplementalGroupsPolicy": "Merge"}, false, false},
		{"supplementalGroupsPolicy Strict", map[string]any{"supplementalGroupsPolicy": "Strict"}, false, true},
		{"seccompProfile RuntimeDefault", map[string]any{"seccompProfile": profile("RuntimeDefault")}, true, false},
		{"seccompProfile Unconfined", map[string]any{"seccompProfile": profile("Unconfined")}, true, true},
		{"seccompProfile Localhost", map[string]any{"seccompProfile": map[string]any{"type": "Localhost", "localhostProfile": "p.json"}}, true, true},
		{"appArmorProfile RuntimeDefault", map[string]any{"appArmorProfile": profile("RuntimeDefault")}, true, false},
		{"appArmorProfile Unconfined", map[string]any{"appArmorProfile": profile("Unconfined")}, true, true},
		{"appArmorProfile Localhost", map[string]any{"appArmorProfile": map[string]any{"type": "Localhost", "localhostProfile": "p"}}, true, true},
		{"seLinuxOptions", map[string]any{"seLinuxOptions": map[string]any{"level": "s0"}}, false, true},
		{"seLinuxChangePolicy", map[string]any{"seLinuxChangePolicy": "Recursive"}, false, true},
		{"sysctls", map[string]any{"sysctls": []any{map[string]any{"name": "a", "value": "b"}}}, false, true},
		{"windowsOptions", map[string]any{"windowsOptions": map[string]any{"runAsUserName": "x"}}, false, true},
		{"unknown key", map[string]any{"somethingNew": true}, false, true},
		{"honoured plus pinned is pinned", map[string]any{"runAsUser": 1, "sysctls": []any{}}, false, true},
		{"nil value is unset", map[string]any{"sysctls": nil}, false, false},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			assert.Equal(t, r.want(), podSecurityNeedsKubernetes(r.sc))
		})
	}
}

func TestContainerSecurityNeedsKubernetes(t *testing.T) {
	caps := func(k string, v ...any) map[string]any {
		return map[string]any{"capabilities": map[string]any{k: v}}
	}
	rows := []secRow{
		{"empty", map[string]any{}, false, false},
		{"runAsUser", map[string]any{"runAsUser": 1000}, false, false},
		{"runAsGroup", map[string]any{"runAsGroup": 1000}, false, false},
		{"runAsNonRoot", map[string]any{"runAsNonRoot": true}, false, false},
		{"allowPrivilegeEscalation false", map[string]any{"allowPrivilegeEscalation": false}, true, false},
		{"allowPrivilegeEscalation true", map[string]any{"allowPrivilegeEscalation": true}, true, false},
		{"capabilities drop ALL", caps("drop", "ALL"), true, false},
		{"capabilities drop several", caps("drop", "NET_RAW", "MKNOD"), true, false},
		{"capabilities add NET_BIND_SERVICE", caps("add", "NET_BIND_SERVICE"), true, false},
		{"capabilities add SYS_ADMIN", caps("add", "SYS_ADMIN"), true, true},
		{"capabilities add ALL", caps("add", "ALL"), true, true},
		{"capabilities unknown key", map[string]any{"capabilities": map[string]any{"grant": []any{"X"}}}, true, true},
		{"privileged false", map[string]any{"privileged": false}, true, false},
		{"privileged true", map[string]any{"privileged": true}, true, true},
		{"readOnlyRootFilesystem true", map[string]any{"readOnlyRootFilesystem": true}, true, false},
		{"readOnlyRootFilesystem false", map[string]any{"readOnlyRootFilesystem": false}, true, false},
		{"procMount Default", map[string]any{"procMount": "Default"}, true, false},
		{"procMount Unmasked", map[string]any{"procMount": "Unmasked"}, true, true},
		{"seccompProfile RuntimeDefault", map[string]any{"seccompProfile": profile("RuntimeDefault")}, true, false},
		{"seccompProfile Unconfined", map[string]any{"seccompProfile": profile("Unconfined")}, true, true},
		{"appArmorProfile RuntimeDefault", map[string]any{"appArmorProfile": profile("RuntimeDefault")}, true, false},
		{"appArmorProfile Localhost", map[string]any{"appArmorProfile": map[string]any{"type": "Localhost", "localhostProfile": "p"}}, true, true},
		{"seLinuxOptions", map[string]any{"seLinuxOptions": map[string]any{"level": "s0"}}, false, true},
		{"windowsOptions", map[string]any{"windowsOptions": map[string]any{"runAsUserName": "x"}}, false, true},
		{"unknown key", map[string]any{"somethingNew": true}, false, true},
		{"fsGroup is not a container field", map[string]any{"fsGroup": 1}, false, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			assert.Equal(t, r.want(), containerSecurityNeedsKubernetes(r.sc))
		})
	}
}

func TestHasSecurityContext(t *testing.T) {
	assert.False(t, HasSecurityContext(nil))
	assert.False(t, HasSecurityContext(&PodTemplate{}))
	assert.False(t, HasSecurityContext(&PodTemplate{Spec: map[string]any{"containers": []any{map[string]any{"name": "a"}}}}))
	assert.False(t, HasSecurityContext(&PodTemplate{Spec: map[string]any{"securityContext": map[string]any{}}}), "empty map is not a securityContext")
	assert.True(t, HasSecurityContext(&PodTemplate{Spec: map[string]any{"securityContext": map[string]any{"runAsUser": 1}}}))
	assert.True(t, HasSecurityContext(&PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "a"},
		map[string]any{"name": "b", "securityContext": map[string]any{"runAsUser": 1}},
	}}}))
}

func secCtxJob(podSC, ctrSC string) string {
	y := "apiVersion: unified-cd/v1\nkind: Job\nmetadata: {name: x}\nspec:\n  podTemplate:\n    spec:\n"
	if podSC != "" {
		y += "      securityContext: " + podSC + "\n"
	}
	y += "      containers: [{name: job, image: img"
	if ctrSC != "" {
		y += ", securityContext: " + ctrSC
	}
	y += "}]\n  steps:\n    - {name: s, run: echo}\n"
	return y
}

func TestParse_SecurityContextValidation(t *testing.T) {
	good := []struct{ pod, ctr string }{
		{"{runAsUser: 1000, runAsGroup: 0, fsGroup: 2147483647, supplementalGroups: [1, 2], fsGroupChangePolicy: OnRootMismatch, supplementalGroupsPolicy: Merge}", "{runAsUser: 5, runAsNonRoot: true}"},
		{"{fsGroupChangePolicy: Always, supplementalGroupsPolicy: Strict}", ""},
		{"", "{runAsUser: 0}"},
		{"{}", "{}"},
	}
	for _, g := range good {
		_, err := Parse(strings.NewReader(secCtxJob(g.pod, g.ctr)))
		assert.NoError(t, err, "%s / %s", g.pod, g.ctr)
	}

	bad := []struct{ name, pod, ctr, want string }{
		{"pod runAsUser string", `{runAsUser: "1000"}`, "", `podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got "1000")`},
		{"pod runAsUser fractional", `{runAsUser: 1000.5}`, "", `podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got 1000.5)`},
		{"pod runAsUser negative", `{runAsUser: -1}`, "", `podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got -1)`},
		{"pod runAsUser too big", `{runAsUser: 2147483648}`, "", `podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got 2147483648)`},
		{"pod runAsGroup", `{runAsGroup: "g"}`, "", `podTemplate.spec.securityContext.runAsGroup: must be an integer`},
		{"pod fsGroup", `{fsGroup: -5}`, "", `podTemplate.spec.securityContext.fsGroup: must be an integer`},
		{"pod supplementalGroups element", `{supplementalGroups: [1, "x"]}`, "", `podTemplate.spec.securityContext.supplementalGroups[1]: must be an integer`},
		{"pod supplementalGroups not a list", `{supplementalGroups: 5}`, "", `podTemplate.spec.securityContext.supplementalGroups: must be a list of integers`},
		{"pod fsGroupChangePolicy", `{fsGroupChangePolicy: Never}`, "", `podTemplate.spec.securityContext.fsGroupChangePolicy: must be one of Always, OnRootMismatch (got "Never")`},
		{"pod supplementalGroupsPolicy", `{supplementalGroupsPolicy: Sometimes}`, "", `podTemplate.spec.securityContext.supplementalGroupsPolicy: must be one of Merge, Strict (got "Sometimes")`},
		{"pod securityContext not a map", `[1]`, "", `podTemplate.spec.securityContext: must be a map`},
		{"container runAsUser", "", `{runAsUser: "root"}`, `podTemplate.spec.containers[0].securityContext.runAsUser: must be an integer between 0 and 2147483647 (got "root")`},
		{"container runAsGroup", "", `{runAsGroup: 3000000000}`, `podTemplate.spec.containers[0].securityContext.runAsGroup: must be an integer`},
		{"container securityContext not a map", "", `5`, `podTemplate.spec.containers[0].securityContext: must be a map`},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(secCtxJob(b.pod, b.ctr)))
			require.Error(t, err)
			assert.Contains(t, err.Error(), b.want)
		})
	}
}

// TestTierBHardeningIsHonouredOnTheHost pins the outcome with literals (the
// table above derives from the constant, so it cannot notice the switch being
// left off): the flag-only hardening fields no longer pin a job to Kubernetes,
// while the widening values still do.
func TestTierBHardeningIsHonouredOnTheHost(t *testing.T) {
	honoured := []map[string]any{
		{"allowPrivilegeEscalation": false},
		{"readOnlyRootFilesystem": true},
		{"capabilities": map[string]any{"drop": []any{"ALL"}}},
		{"capabilities": map[string]any{"drop": []any{"ALL"}, "add": []any{"NET_BIND_SERVICE"}}},
		{"seccompProfile": profile("RuntimeDefault")},
		{"privileged": false},
	}
	for _, sc := range honoured {
		assert.False(t, containerSecurityNeedsKubernetes(sc), "%v", sc)
	}
	assert.False(t, podSecurityNeedsKubernetes(map[string]any{"seccompProfile": profile("RuntimeDefault")}))
	pinned := []map[string]any{
		{"capabilities": map[string]any{"add": []any{"SYS_ADMIN"}}},
		{"seccompProfile": profile("Unconfined")},
		{"privileged": true},
	}
	for _, sc := range pinned {
		assert.True(t, containerSecurityNeedsKubernetes(sc), "%v", sc)
	}
	assert.True(t, podSecurityNeedsKubernetes(map[string]any{"seccompProfile": profile("Unconfined")}))
}
