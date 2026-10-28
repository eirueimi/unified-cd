package k8sagent

import (
	"fmt"
	"path"
	"strings"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// sidecarProtectedDirs are the artifact sidecar's own credential mount
// directories. They sit under /etc and /var/run, so dsl.ProtectedMountDirs
// already covers them; they are listed again so the guarantee does not depend
// on where the constants happen to point.
var sidecarProtectedDirs = []string{
	sidecarBrokerTokenMountPath,
	sidecarS3CredentialsMountPath,
}

// CheckJobMountPaths refuses a claim whose job-chosen mount paths could put
// job-writable content inside the artifact sidecar's filesystem view. The
// workspace mountPath and every absolute spec.volumes mountPath are mounted into
// EVERY container of the Pod, the sidecar included; the sidecar holds the
// object-store credentials and runs /usr/local/bin/unified-sidecar, so a mount
// on (or over) its binaries, libraries, configuration, credential mounts or the
// /.ucd volume must never happen. The protected list is dsl.ProtectedMountDirs
// plus sidecarProtectedDirs; no jobPodPolicy setting allows these paths.
//
// Only the JOB's own workspace.mountPath is checked: agent-side podTemplates
// are operator-written and trusted. A relative spec.volumes mountPath resolves
// under the workspace and is covered by the workspace check, except that one
// which climbs out of it ("..") is resolved and checked here as a backstop for
// jobs stored before apply-time validation rejected that.
func CheckJobMountPaths(jobTmpl *dsl.PodTemplate, workspaceMount string, volumes []dsl.VolumeMount) error {
	var vs []PodPolicyViolation
	deny := func(where string, err error) {
		vs = append(vs, PodPolicyViolation{Path: where, What: err.Error()})
	}
	if jobTmpl != nil && jobTmpl.Workspace != nil && jobTmpl.Workspace.MountPath != "" {
		mp := jobTmpl.Workspace.MountPath
		if err := dsl.CheckContainerMountPath(mp, sidecarProtectedDirs...); err != nil {
			deny("podTemplate.workspace.mountPath", err)
		}
	}
	for i, v := range volumes {
		mp := v.MountPath
		where := fmt.Sprintf("spec.volumes[%d].mountPath", i)
		switch {
		case path.IsAbs(mp):
			if err := dsl.CheckContainerMountPath(mp, sidecarProtectedDirs...); err != nil {
				deny(where, err)
			}
		case path.Clean(mp) == ".." || strings.HasPrefix(path.Clean(mp), "../"):
			resolved := path.Join(workspaceMount, mp)
			if err := dsl.CheckContainerMountPath(resolved, sidecarProtectedDirs...); err != nil {
				deny(where, err)
			}
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return &JobPodPolicyError{Violations: vs}
}
