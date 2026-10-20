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

// MountedVolume is a named volume mounted at a relative path inside the
// workspace, located in the coordinates of the archiving step: a host path on a
// host agent, a container path on the Kubernetes agent.
type MountedVolume struct {
	Volume    string // the volume's name
	MountPath string // as the job declared it: cleaned, slash-separated, relative to the workspace
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

// VolumeLocations lists where each of vols is, for the archiver's exclusion list.
func VolumeLocations(vols []MountedVolume) []string {
	locs := make([]string, len(vols))
	for i, v := range vols {
		locs[i] = v.Location
	}
	return locs
}

// VolumeExclusionNotice is the run-log line for one volume left out of an
// archive. kind is "artifact" or "cache"; shown is the archived path as the job
// would write it (relative to the workspace), so the line reads the same on
// every agent.
func VolumeExclusionNotice(kind, shown string, v MountedVolume) string {
	return fmt.Sprintf("%s path %q excludes volume %q mounted at %q: the contents of a volume are never archived as part of a parent directory; name the volume's mount path to archive it",
		kind, shown, v.Volume, v.MountPath)
}
