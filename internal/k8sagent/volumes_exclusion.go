package k8sagent

import (
	"context"
	"path"
	"strings"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
)

// mountedVolumes lists the volumes mounted at a relative path inside the
// workspace, located as the artifact sidecar sees them. The workspace root and
// an absolute container path are left out for the same reason the host agent
// leaves them out: neither is a directory inside the workspace to be archived
// by an ancestor path (see agentlib.MountedVolume).
func mountedVolumes(workspace string, vols []PodVolume) []agentlib.MountedVolume {
	var out []agentlib.MountedVolume
	for _, v := range vols {
		if v.MountPath == "." || path.IsAbs(v.MountPath) {
			continue
		}
		out = append(out, agentlib.MountedVolume{
			Volume:    v.Name,
			MountPath: v.MountPath,
			Location:  path.Join(workspace, v.MountPath),
		})
	}
	return out
}

// shownPath is resolved, an artifact or cache path as ResolveArtifactPath
// returned it, written as the job would: relative to the workspace.
func (b *k8sBackend) shownPath(resolved string) string {
	root := path.Clean(b.mountPath)
	if resolved == root {
		return "."
	}
	if rel, ok := strings.CutPrefix(resolved, root+"/"); ok {
		return rel
	}
	return resolved
}

// excludeVolumes returns the --exclude arguments an archive of resolved needs:
// the mount point of every volume strictly below it, which the sidecar sees as
// part of the workspace and would otherwise archive in full. It announces each
// in the run log, in the words the host agent uses.
func (b *k8sBackend) excludeVolumes(ctx context.Context, kind, resolved string) []string {
	below := agentlib.VolumesBelow(resolved, b.volumes)
	var args []string
	for _, v := range below {
		b.a.warnRun(ctx, b.runID, agentlib.VolumeExclusionNotice(kind, b.shownPath(resolved), v))
		args = append(args, "--exclude", v.Location)
	}
	return args
}
