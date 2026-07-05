package k8sagent

import "testing"

func TestBuildScopePodHasScratchAndSidecarNoWorkspacePVC(t *testing.T) {
	pod := buildScopePod("run123", "ci", "scope:build", "golang:1.22",
		map[string]string{"K": "V"}, SidecarSpec{Image: "sidecar:1", S3SecretName: "s3"})

	// scratch volume is emptyDir, mounted by both the step and the sidecar
	var scratch *string
	for _, v := range pod.Spec.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Fatal("scope scratch volume must be emptyDir, not a PVC")
			}
			n := v.Name
			scratch = &n
		}
	}
	if scratch == nil {
		t.Fatal("missing scratch volume")
	}
	names := map[string]bool{}
	for _, c := range pod.Spec.Containers {
		names[c.Name] = true
		mounted := false
		for _, m := range c.VolumeMounts {
			if m.Name == "workspace" {
				mounted = true
			}
		}
		if !mounted {
			t.Fatalf("container %q does not mount the scratch volume", c.Name)
		}
	}
	if !names["step"] {
		t.Fatal("missing step container")
	}
}
