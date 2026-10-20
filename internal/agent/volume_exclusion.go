package agent

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// A volume's contents are not part of an archive of one of its ancestor
// directories. A job that declares .ucd-cache/go as a persistent volume and then
// archives "." wants its workspace, not the build cache it keeps precisely so
// that it is NOT rebuilt (and re-uploaded) every run. Both agents apply this
// rule, so the same job produces the same archive on either, and both announce
// it in the run log: silence is the one thing the rule must not be.
//
// An archive of the mount point itself, or of anything inside it, is a
// deliberate choice of that volume and archives it (see resolveInVolumeMount).

// MountedVolume is a named volume mounted inside the workspace, located where
// the job put it: Location is the mount point in the workspace's own coordinates
// (a workspace directory on a host agent, the pod's workspace path on the
// Kubernetes agent), in the same form as the path being archived. Nesting is
// judged in these coordinates on both agents, whatever the storage layout.
type MountedVolume struct {
	Volume    string // the volume's name
	MountPath string // the mount path as the job declared it, cleaned and slash-separated
	Location  string // where the mount point is, in the same form as the path being archived
}

// VolumesBelow returns the volumes whose mount point lies strictly below
// archivePath, ordered by location. The result is what an archive of
// archivePath must leave out.
func VolumesBelow(archivePath string, vols []MountedVolume) []MountedVolume {
	root := path.Clean(filepath.ToSlash(archivePath))
	var out []MountedVolume
	for _, v := range vols {
		loc := path.Clean(filepath.ToSlash(v.Location))
		if loc != root && strings.HasPrefix(loc, strings.TrimSuffix(root, "/")+"/") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Location < out[j].Location })
	return out
}

// VolumeExclusionNotice is the run-log line for one volume left out of an
// archive, or left alone by an extraction. kind is "artifact", "artifact
// download" or "cache ..."; shown is the archived path as the job would write it
// (relative to the workspace), so the line reads the same on every agent.
func VolumeExclusionNotice(kind, shown string, v MountedVolume) string {
	if kind == "artifact download" {
		return fmt.Sprintf("%s path %q excludes volume %q mounted at %q: files of the artifact that lie inside a volume are not extracted into it; name the volume's mount path as the destination to extract into it",
			kind, shown, v.Volume, v.MountPath)
	}
	return fmt.Sprintf("%s path %q excludes volume %q mounted at %q: the contents of a volume are never archived as part of a parent directory; name the volume's mount path to archive it",
		kind, shown, v.Volume, v.MountPath)
}
