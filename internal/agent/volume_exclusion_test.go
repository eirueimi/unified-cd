package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// exclusionScenario is one entry of testdata/volume_exclusion_golden.json: the
// one statement of the rule that the host agent and the Kubernetes agent are
// both held to.
type exclusionScenario struct {
	Mounts []struct {
		Name      string `json:"name"`
		MountPath string `json:"mountPath"`
	} `json:"mounts"`
	ArchivePath    string              `json:"archivePath"`
	WorkspaceFiles []string            `json:"workspaceFiles"`
	VolumeFiles    []string            `json:"volumeFiles"`
	Entries        []string            `json:"entries"`
	Notices        map[string][]string `json:"notices"`
	Excluded       []string            `json:"excluded"`
}

// exclusionScenarios reads the shared golden file.
func exclusionScenarios(t *testing.T) map[string]exclusionScenario {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "volume_exclusion_golden.json"))
	require.NoError(t, err)
	var g struct {
		Scenarios map[string]exclusionScenario `json:"scenarios"`
	}
	require.NoError(t, json.Unmarshal(b, &g))
	return g.Scenarios
}

// scenarioNames returns the scenario names in a stable order.
func scenarioNames(t *testing.T) []string {
	var names []string
	for n := range exclusionScenarios(t) {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func TestVolumesBelow(t *testing.T) {
	vols := []MountedVolume{
		{Volume: "b", MountPath: "a/b", Location: "/w/a/b"},
		{Volume: "a", MountPath: "a", Location: "/w/a"},
		{Volume: "c", MountPath: "ab", Location: "/w/ab"},
	}
	names := func(path string) []string {
		var out []string
		for _, v := range VolumesBelow(path, vols) {
			out = append(out, v.Volume)
		}
		return out
	}
	require.Equal(t, []string{"a", "b", "c"}, names("/w"), "the workspace holds every mount point, ordered by location")
	require.Equal(t, []string{"b"}, names("/w/a"), "the mount point itself is archived, not excluded; only what is nested below it is")
	require.Empty(t, names("/w/a/b"), "archiving a volume's own root excludes nothing")
	require.Empty(t, names("/w/a/b/x"))
	require.Equal(t, []string{"a", "b", "c"}, names("/w/"), "a trailing separator changes nothing")
	require.Empty(t, names("/w/ab"), "a near miss by name is not an ancestor")
	require.Equal(t, []string{"a", "b", "c"}, names("/"), "the filesystem root is an ancestor of everything")
}
