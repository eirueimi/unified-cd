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
// are operator-written and trusted. Every spec.volumes mountPath is first put
// through the same normalisation the mount uses (backslashes are separators,
// then Clean, then a relative path is joined under the workspace mount) and the
// RESULT is checked, so no spelling of a protected path gets past. A path that
// lands under the workspace is covered by the workspace check; one that climbs
// out of it ("..") is checked here as a backstop for jobs stored before
// apply-time validation rejected that, and so is one with a backslash, which
// apply-time validation now rejects.
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
		where := fmt.Sprintf("spec.volumes[%d].mountPath", i)
		// Check the path the agent will mount, not the string as written: a
		// backslash is a separator there ("\usr\local\bin" is /usr/local/bin).
		mp := normalizeVolumeMountPath(v.MountPath)
		if mp == "." {
			continue // the workspace root; the workspace check covers it
		}
		wm := path.Clean(workspaceMount)
		resolved := volumeContainerPath(wm, mp)
		if !path.IsAbs(mp) && (resolved == wm || strings.HasPrefix(resolved, strings.TrimSuffix(wm, "/")+"/")) {
			continue // under the workspace; the workspace check covers it
		}
		if err := dsl.CheckContainerMountPath(resolved, sidecarProtectedDirs...); err != nil {
			deny(where, err)
		}
	}
	if len(vs) == 0 {
		return nil
	}
	return &JobPodPolicyError{Violations: vs}
}
