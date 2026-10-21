package k8sagent

import (
	"context"
	"path"
	"strings"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
)

// mountedVolumes lists the volumes mounted below the workspace root, located
// where they land in the container as the artifact sidecar sees them. How the
// mount path was written does not matter: "cache" and "/src/cache" are one
// place under a workspace at /src. The workspace root is the workspace itself;
// a volume that lands outside the workspace is listed too, and is simply never
// below an archived path (agentlib.VolumesBelow).
func mountedVolumes(workspace string, vols []PodVolume) []agentlib.MountedVolume {
	var out []agentlib.MountedVolume
	for _, v := range vols {
		loc := path.Clean(v.MountPath)
		switch {
		case v.MountPath == ".":
			continue
		case !path.IsAbs(loc):
			loc = path.Join(workspace, loc)
		}
		out = append(out, agentlib.MountedVolume{Volume: v.Name, MountPath: v.MountPath, Location: loc})
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
