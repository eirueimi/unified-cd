package paritycases

import (
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// SecurityContextImage is the image every securityContext case runs in. It is
// small, has no USER, and its /etc/passwd and /etc/group give the cases below
// something to find (www-data is 33:33) and something not to find (1234).
const SecurityContextImage = "docker.io/library/busybox:1.36"

// SecurityContextCases returns the real-runtime securityContext parity cases
// (design §10). They are kept apart from Cases(): a fake shell cannot observe
// UIDs, so these only mean something against a real container runtime (the
// host driver, internal/agent/secctx_parity_integration_test.go) or a real
// kubelet (the k8s driver, internal/k8sagent/secctx_parity_k8s_test.go). Both
// drivers assert the SAME Expectation, which is the parity claim.
//
// Every claim prints fixed-format "key=value" lines, so an Expectation is a set
// of LogMustContain substrings.
func SecurityContextCases() []Case {
	return []Case{
		secPodLevel(),
		secContainerOverride(),
		secUIDOnlyWithPasswdEntry(),
		secUIDOnlyWithoutPasswdEntry(),
		secArtifactPodLevel(),
		secArtifactContainerLevel(),
		secVolumeRegroupAlways(),
		secVolumeOnRootMismatchSkips(),
		secNoSecurityContext(),
	}
}

// identNoFSGroup and identFSGroup print the process identity and what it makes.
// groups is sorted so the order the runtimes list them in does not matter.
// identFSGroup also prints the workspace's group, which only means something
// when fsGroup is set (without it the host workspace belongs to the agent and
// the k8s emptyDir to root, a documented difference: design §9).
const identNoFSGroup = `echo "uid=$(id -u)"
echo "gid=$(id -g)"
echo "groups=$(id -G | tr ' ' '\n' | sort -n | tr '\n' ',')"
echo "ws=$(stat -c '%a' "$UNIFIED_WORKSPACE")"
rm -f probe-file; touch probe-file
echo "file=$(stat -c '%u %g' probe-file)"`

const identFSGroup = `echo "uid=$(id -u)"
echo "gid=$(id -g)"
echo "groups=$(id -G | tr ' ' '\n' | sort -n | tr '\n' ',')"
echo "ws=$(stat -c '%a %g' "$UNIFIED_WORKSPACE")"
rm -f probe-file; touch probe-file
echo "file=$(stat -c '%u %g' probe-file)"`

// jobContainer is the explicit primary container. The host agent injects one
// when absent, but BuildPod on Kubernetes does not, so the cases name it.
func jobContainer() map[string]any {
	return map[string]any{"name": "job", "image": SecurityContextImage}
}

func podTemplate(podSC map[string]any, containers ...any) *dsl.PodTemplate {
	spec := map[string]any{"containers": append([]any{jobContainer()}, containers...)}
	if podSC != nil {
		spec["securityContext"] = podSC
	}
	return &dsl.PodTemplate{Spec: spec}
}

func step(i int, name, run string) api.ClaimStage {
	return api.ClaimStage{Step: &api.ClaimStep{Index: i, StageIndex: i, Name: name, Run: run}}
}

func containerStep(i int, name, container, run string) api.ClaimStage {
	return api.ClaimStage{Step: &api.ClaimStep{Index: i, StageIndex: i, Name: name, Run: run, Container: container}}
}

// expectStdout builds an Expectation: every named step Succeeded, the run
// Succeeded, and each line present on its step's stdout.
func expectStdout(steps []string, lines map[string][]string) Expectation {
	e := Expectation{StepStatus: map[string]string{}, RunFinished: "Succeeded"}
	for _, s := range steps {
		e.StepStatus[s] = "Succeeded"
	}
	for s, ls := range lines {
		for _, l := range ls {
			e.LogMustContain = append(e.LogMustContain, LogLine{Step: s, Stream: "stdout", Substring: l})
		}
	}
	return e
}

func isolatedClaim(runID string, podTpl *dsl.PodTemplate, vols []dsl.VolumeMount, stages ...api.ClaimStage) api.ClaimResponse {
	return api.ClaimResponse{
		RunID:       runID,
		JobName:     runID,
		PodTemplate: podTpl,
		Volumes:     vols,
		Stages:      stages,
	}
}

// (a) pod-level runAsUser, runAsGroup, fsGroup and supplementalGroups.
func secPodLevel() Case {
	return Case{
		Name: "secctx-pod-level",
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-pod-level", podTemplate(map[string]any{
				"runAsUser": 1000, "runAsGroup": 2000, "fsGroup": 3000,
				"supplementalGroups": []any{4000, 5000},
			}), nil, step(0, "ident", identFSGroup))
		},
		Expect: expectStdout([]string{"ident"}, map[string][]string{"ident": {
			"uid=1000", "gid=2000", "groups=2000,3000,4000,5000,",
			"ws=2777 3000", "file=1000 3000",
		}}),
	}
}

// (b) a container-level securityContext on one sidecar overrides the pod's; the
// second step targets it with container:. fsGroup and supplementalGroups stay
// pod-level, so the sidecar still holds fsGroup as a supplementary group.
func secContainerOverride() Case {
	side := map[string]any{
		"name": "side", "image": SecurityContextImage,
		"command":         []any{"sleep", "infinity"},
		"securityContext": map[string]any{"runAsUser": 1500, "runAsGroup": 2500},
	}
	return Case{
		Name: "secctx-container-override",
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-container-override", podTemplate(map[string]any{
				"runAsUser": 1000, "runAsGroup": 2000, "fsGroup": 3000,
			}, side), nil,
				step(0, "job-ident", identFSGroup),
				containerStep(1, "side-ident", "side", identFSGroup))
		},
		Expect: expectStdout([]string{"job-ident", "side-ident"}, map[string][]string{
			"job-ident":  {"uid=1000", "gid=2000", "groups=2000,3000,", "ws=2777 3000", "file=1000 3000"},
			"side-ident": {"uid=1500", "gid=2500", "groups=2500,3000,", "ws=2777 3000", "file=1500 3000"},
		}),
	}
}

// (c1) runAsUser only, with an image that has a passwd entry for it: the
// primary group comes from /etc/passwd and /etc/group (V2).
func secUIDOnlyWithPasswdEntry() Case {
	return Case{
		Name: "secctx-uid-only-passwd-entry",
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-uid-passwd", podTemplate(map[string]any{"runAsUser": 33}),
				nil, step(0, "ident", identNoFSGroup))
		},
		Expect: expectStdout([]string{"ident"}, map[string][]string{"ident": {
			"uid=33", "gid=33", "groups=33,", "ws=777", "file=33 33",
		}}),
	}
}

// (c2) runAsUser only, with no passwd entry for it: the primary group is 0 (V2).
func secUIDOnlyWithoutPasswdEntry() Case {
	return Case{
		Name: "secctx-uid-only-no-passwd-entry",
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-uid-nopasswd", podTemplate(map[string]any{"runAsUser": 1234}),
				nil, step(0, "ident", identNoFSGroup))
		},
		Expect: expectStdout([]string{"ident"}, map[string][]string{"ident": {
			"uid=1234", "gid=0", "groups=0,", "ws=777", "file=1234 0",
		}}),
	}
}

const artifactCheck = `echo "dl=$(stat -c '%u %g' dl/a.txt)"
{ echo y >> dl/a.txt; } 2>/dev/null && echo "write=ok" || echo "write=denied"`

func artifactClaim(runID string, podTpl *dsl.PodTemplate) api.ClaimResponse {
	return isolatedClaim(runID, podTpl, nil,
		api.ClaimStage{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "fetch",
			DownloadArtifact: &api.DownloadArtifactStep{Name: ArtifactName, DestDir: "dl"},
		}},
		step(1, "check", artifactCheck))
}

var artifactFiles = map[string]string{"a.txt": "artifact\n"}

// (d1) pod-level identity: the platform writes the download as that identity,
// so the job can modify it (§6.4).
func secArtifactPodLevel() Case {
	return Case{
		Name: "secctx-artifact-pod-level",
		Claim: func() api.ClaimResponse {
			return artifactClaim("run-secctx-artifact-pod", podTemplate(map[string]any{
				"runAsUser": 1000, "fsGroup": 1000,
			}))
		},
		Artifacts: artifactFiles,
		Expect: expectStdout([]string{"fetch", "check"}, map[string][]string{"check": {
			"dl=1000 1000", "write=ok",
		}}),
	}
}

// (d2) identity only on the job container: the artifact sidecar does not get
// it, so the download is root-owned and the job cannot modify it (§6.4).
func secArtifactContainerLevel() Case {
	job := map[string]any{
		"name": "job", "image": SecurityContextImage,
		"securityContext": map[string]any{"runAsUser": 1000},
	}
	return Case{
		Name: "secctx-artifact-container-level",
		Claim: func() api.ClaimResponse {
			tpl := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{job}}}
			return artifactClaim("run-secctx-artifact-container", tpl)
		},
		Artifacts: artifactFiles,
		Expect: expectStdout([]string{"fetch", "check"}, map[string][]string{"check": {
			"dl=0 0", "write=denied",
		}}),
	}
}

var dataVolume = []dsl.VolumeMount{{Name: dsl.StringOrSlice{"data"}, MountPath: "/data"}}

const hostOnlyVolumeLeftover = "a leftover root-owned file in a volume needs an earlier run's output to survive: " +
	"kubelet applies fsGroup when it sets the volume up, before any container writes, " +
	"and the kind cluster's local-path PVCs are never regrouped (design §6.5)"

// seedRootFile has a root container (no securityContext) leave a root-owned
// 0644 file in the volume, as an earlier run of the job would have.
func seedRootFile(runID, file string) Case {
	return Case{
		Name: runID,
		Claim: func() api.ClaimResponse {
			return isolatedClaim(runID, podTemplate(nil), dataVolume, step(0, "seed",
				"echo x > /data/"+file+"\nchmod 644 /data/"+file+"\necho \"seed=$(stat -c '%u %g %a' /data/"+file+")\""))
		},
		Expect: expectStdout([]string{"seed"}, map[string][]string{"seed": {"seed=0 "}}),
	}
}

// (e1) fsGroup under the default policy (Always) regroups what an earlier root
// run left behind, and a runAsUser step can then write it (§6.5).
func secVolumeRegroupAlways() Case {
	return Case{
		Name:     "secctx-volume-regroup-always",
		HostOnly: hostOnlyVolumeLeftover,
		Setup:    []Case{seedRootFile("run-secctx-vol-always-seed", "left.txt")},
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-vol-always", podTemplate(map[string]any{
				"runAsUser": 1000, "fsGroup": 4000,
			}), dataVolume, step(0, "check", `echo "left=$(stat -c '%a %g' /data/left.txt)"
echo "root=$(stat -c '%a %g' /data)"
{ echo more >> /data/left.txt; } 2>/dev/null && echo "append=ok" || echo "append=denied"`))
		},
		Expect: expectStdout([]string{"check"}, map[string][]string{"check": {
			"left=664 4000", "root=2775 4000", "append=ok",
		}}),
	}
}

// (e2) fsGroupChangePolicy OnRootMismatch skips the walk when the volume root
// already carries the group, setgid and g+rwx. A file added afterwards by a
// root run (group inherited through the setgid root, mode untouched) keeps its
// 0644, while Always would have made it 0664 (§6.1, §6.5).
func secVolumeOnRootMismatchSkips() Case {
	podSC := func(policy string) map[string]any {
		return map[string]any{"runAsUser": 1000, "fsGroup": 4000, "fsGroupChangePolicy": policy}
	}
	regroup := Case{
		Name: "run-secctx-vol-skip-regroup",
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-vol-skip-regroup", podTemplate(podSC("Always")), dataVolume,
				step(0, "regroup", `echo "left=$(stat -c '%a %g' /data/left.txt)"`))
		},
		Expect: expectStdout([]string{"regroup"}, map[string][]string{"regroup": {"left=664 4000"}}),
	}
	// A root run in the now-setgid volume: the new file's group is inherited.
	seed2 := seedRootFile("run-secctx-vol-skip-seed2", "left2.txt")
	seed2.Expect = expectStdout([]string{"seed"}, map[string][]string{"seed": {"seed=0 4000 644"}})
	return Case{
		Name:     "secctx-volume-onrootmismatch-skips",
		HostOnly: hostOnlyVolumeLeftover,
		Setup:    []Case{seedRootFile("run-secctx-vol-skip-seed1", "left.txt"), regroup, seed2},
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-vol-skip", podTemplate(podSC("OnRootMismatch")), dataVolume,
				step(0, "check", `echo "left2=$(stat -c '%a %g' /data/left2.txt)"
{ echo more >> /data/left2.txt; } 2>/dev/null && echo "append2=ok" || echo "append2=denied"`))
		},
		Expect: expectStdout([]string{"check"}, map[string][]string{"check": {
			"left2=644 4000", "append2=denied",
		}}),
	}
}

// (f) no securityContext: a root image runs as root, as on Kubernetes
// (decision 1). The host adds no identity of its own.
func secNoSecurityContext() Case {
	return Case{
		Name: "secctx-none-root-image",
		Claim: func() api.ClaimResponse {
			return isolatedClaim("run-secctx-none", podTemplate(nil), nil, step(0, "ident", identNoFSGroup))
		},
		Expect: expectStdout([]string{"ident"}, map[string][]string{"ident": {
			"uid=0", "gid=0", "groups=0,10,", "ws=777", "file=0 0",
		}}),
	}
}
