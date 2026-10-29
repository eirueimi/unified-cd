//go:build k8s

package k8sagent

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/paritycases"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// artifactEmuContainer stands in for the real unified-artifact sidecar, which
// needs the sidecar image and an object store. What §6.4 of the design says
// about it is that it runs with the POD-level securityContext and no
// container-level one of its own, writing into the workspace. A plain busybox
// container in the pod, given nothing but the pod's securityContext, has
// exactly that identity, so the cases' ownership expectations hold for it.
//
// That equivalence is an INVARIANT, not something this test proves: the
// emulator has no securityContext by construction, so artifact case d2 would
// pass whatever BuildPod did to the real sidecar. The invariant is pinned by
// TestBuildPod_ArtifactSidecarHasNoContainerSecurityContext
// (podbuilder_test.go), which fails if BuildPod ever gives the real sidecar a
// container-level securityContext. If it does, this emulation and the host's
// chown-as rule (claim_pod_security.go agentWriteOwner) must change together.
const artifactEmuContainer = "artifact-emu"

// secCtxK8sBackend runs a case's steps by exec'ing into a real pod on a real
// kubelet. It reuses the parity driver's fake-controller plumbing (log
// shipping, step reports, the embedded parityK8sBackend) and replaces only the
// parts that touch the pod: where a step runs, where the workspace is, and how
// an artifact download is performed.
type secCtxK8sBackend struct {
	*parityK8sBackend
	exec      *Executor
	pod       string
	artifacts map[string]string
}

func (b *secCtxK8sBackend) RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.exec.ExecStep(ctx, b.pod, step.Container, script, step.Shell, env, stdout, stderr)
}

func (b *secCtxK8sBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return b.exec.ExecStep(ctx, b.pod, container, script, step.Shell, env, stdout, stderr)
}

func (b *secCtxK8sBackend) WorkspacePath(scope agentlib.ScopeHandle) string { return "/workspace" }

// DownloadArtifact writes the case's files into destDir from the emulated
// artifact sidecar container (see artifactEmuContainer), as that container's
// pod-level identity.
func (b *secCtxK8sBackend) DownloadArtifact(ctx context.Context, scope agentlib.ScopeHandle, runID, name, destDir string) error {
	if name != paritycases.ArtifactName {
		return fmt.Errorf("unknown artifact %q", name)
	}
	names := make([]string, 0, len(b.artifacts))
	for n := range b.artifacts {
		names = append(names, n)
	}
	sort.Strings(names)
	script := "mkdir -p " + shQuote(destDir)
	for _, n := range names {
		script += " && printf '%s' " + shQuote(b.artifacts[n]) + " > " + shQuote(destDir+"/"+n)
	}
	var out strings.Builder
	code, err := b.exec.ExecStep(ctx, b.pod, artifactEmuContainer, script, nil, nil, &out, &out)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("emulated artifact download exited %d: %s", code, out.String())
	}
	return nil
}

// claimTestPVCs gives each volume a case's claim mounts a fresh PVC in the test
// namespace (the kind cluster's default StorageClass provisions it once the pod
// is scheduled), so the pod mounts it exactly as an agent-owned volume would.
// Without it the case's mountPath is just a directory of the image's root
// filesystem, which readOnlyRootFilesystem makes read-only.
func claimTestPVCs(ctx context.Context, t *testing.T, client kubernetes.Interface, ns string, mounts []dsl.VolumeMount) []PodVolume {
	t.Helper()
	var vols []PodVolume
	for i, m := range mounts {
		require.Len(t, m.Name, 1, "parity cases name one literal volume")
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("parity-vol-%d", i), Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("16Mi")},
				},
			},
		}
		_, err := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, pvc, metav1.CreateOptions{})
		require.NoError(t, err)
		vols = append(vols, PodVolume{Name: m.Name[0], ClaimName: pvc.Name, MountPath: m.MountPath})
	}
	return vols
}

// TestSecCtxParity_K8s drives the same paritycases.SecurityContextCases the
// host driver runs, against a real pod on the kind cluster, and asserts the
// SAME Expectation. A case the host satisfies and the kubelet does not (or the
// reverse) is a parity defect in the design, not in the test.
func TestSecCtxParity_K8s(t *testing.T) {
	shimImage := testShimImageOrSkip(t)
	client, restCfg := newTestKubeClient(t)

	for _, tc := range paritycases.SecurityContextCases() {
		t.Run(tc.Name, func(t *testing.T) {
			if tc.HostOnly != "" {
				t.Skip(tc.HostOnly)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()

			ns := newTestNamespace(t, client)
			pm := NewPodManager(client, ns, paritycases.SecurityContextImage)

			claim := tc.Claim()
			if tc.Artifacts != nil {
				spec := claim.PodTemplate.Spec
				containers, _ := spec["containers"].([]any)
				spec["containers"] = append(containers, map[string]any{
					"name": artifactEmuContainer, "image": paritycases.SecurityContextImage,
					"command": []any{"sleep", "infinity"},
				})
			}
			vols := claimTestPVCs(ctx, t, client, ns, claim.Volumes)
			pod, err := BuildPod(claim.RunID, ns, nil, claim.PodTemplate, paritycases.SecurityContextImage, SidecarSpec{}, shimImage, vols)
			require.NoError(t, err)
			created, err := pm.CreatePod(ctx, pod)
			require.NoError(t, err)
			t.Cleanup(func() { _ = pm.DeletePod(context.Background(), created.Name) })
			require.NoError(t, pm.WaitForPodRunning(ctx, created.Name))

			agentID := "secctx-k8s-" + sanitizeNameK8s(tc.Name)
			h := newParityK8sHarness()
			srv := newParityK8sServer(t, agentID, h)
			c := agentlib.NewClient(srv.URL, "tok")
			backend := &secCtxK8sBackend{
				parityK8sBackend: &parityK8sBackend{t: t, client: c, agentID: agentID, runID: claim.RunID, workDir: t.TempDir()},
				exec:             NewExecutor(client, restCfg, ns),
				pod:              created.Name,
				artifacts:        tc.Artifacts,
			}
			agentlib.RunClaim(ctx, c, agentID, claim, backend)

			paritycases.Assert(t, tc.Expect, h.observation())
		})
	}
}

// TestSecCtx_K8s_PodRunAsNonRootBlocksInjectedRootContainers is design §11 V8
// (and documents Q8). A pod-level runAsNonRoot without a pod-level runAsUser
// makes the kubelet refuse every container whose image runs as root, and the
// shim image's USER is empty (root). So the injected ucd-shim init container
// never starts: this is the k8s-agent defect Q8 records, and the reason the
// host, which has no such container, deliberately does not emulate it.
func TestSecCtx_K8s_PodRunAsNonRootBlocksInjectedRootContainers(t *testing.T) {
	shimImage := testShimImageOrSkip(t)
	client, _ := newTestKubeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, paritycases.SecurityContextImage)
	tpl := &dsl.PodTemplate{Spec: map[string]any{
		"securityContext": map[string]any{"runAsNonRoot": true},
		"containers":      []any{map[string]any{"name": "job", "image": paritycases.SecurityContextImage}},
	}}
	pod, err := BuildPod(uniqueRunID("nonroot"), ns, nil, tpl, paritycases.SecurityContextImage, SidecarSpec{}, shimImage, nil)
	require.NoError(t, err)
	created, err := pm.CreatePod(ctx, pod)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pm.DeletePod(context.Background(), created.Name) })

	var last string
	require.Eventually(t, func() bool {
		p, err := client.CoreV1().Pods(ns).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, st := range append(append([]corev1.ContainerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			if w := st.State.Waiting; w != nil {
				last = fmt.Sprintf("%s: %s: %s", st.Name, w.Reason, w.Message)
				if w.Reason == "CreateContainerConfigError" && strings.Contains(w.Message, "runAsNonRoot") {
					return true
				}
			}
		}
		return false
	}, 2*time.Minute, time.Second, "no container was blocked by runAsNonRoot; last waiting state: %s", last)
	t.Logf("blocked as documented (Q8): %s", last)
}

// TestSecCtx_K8s_FreshEmptyDirRootWithFSGroup checks what the kubelet does to
// the root of a fresh emptyDir under fsGroup: it is created 0777 and the walk
// adds the group and setgid, giving "2777 F" (design 6.1, V1). The host's
// named-volume equivalent starts from 0755, hence the design's "2775 F" row
// for a volume root; this pins the k8s side's exact mask.
func TestSecCtx_K8s_FreshEmptyDirRootWithFSGroup(t *testing.T) {
	shimImage := testShimImageOrSkip(t)
	client, restCfg := newTestKubeClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	ns := newTestNamespace(t, client)
	pm := NewPodManager(client, ns, paritycases.SecurityContextImage)
	tpl := &dsl.PodTemplate{Spec: map[string]any{
		"securityContext": map[string]any{"runAsUser": 1000, "fsGroup": 4000},
		"volumes":         []any{map[string]any{"name": "scratch", "emptyDir": map[string]any{}}},
		"containers": []any{map[string]any{
			"name": "job", "image": paritycases.SecurityContextImage,
			"volumeMounts": []any{map[string]any{"name": "scratch", "mountPath": "/scratch"}},
		}},
	}}
	pod, err := BuildPod(uniqueRunID("emptydir"), ns, nil, tpl, paritycases.SecurityContextImage, SidecarSpec{}, shimImage, nil)
	require.NoError(t, err)
	created, err := pm.CreatePod(ctx, pod)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pm.DeletePod(context.Background(), created.Name) })
	require.NoError(t, pm.WaitForPodRunning(ctx, created.Name))

	var out strings.Builder
	code, err := NewExecutor(client, restCfg, ns).ExecStep(ctx, created.Name, "job", `stat -c '%a %g' /scratch`, nil, nil, &out, &out)
	require.NoError(t, err)
	require.Equal(t, 0, code, out.String())
	require.Equal(t, "2777 4000", strings.TrimSpace(out.String()))
}
