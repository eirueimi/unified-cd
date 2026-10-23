package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const minFreeEnv = "UNIFIED_AGENT_MIN_FREE_DISK"

func effMinFree(t *testing.T, env, yaml string) DiskThreshold {
	t.Helper()
	t.Setenv(minFreeEnv, env)
	path := ""
	if yaml != "" {
		path = writeAgentYAML(t, yaml)
	}
	eff, err := AgentEffective(path)
	require.NoError(t, err)
	return eff.MinFreeDisk
}

func TestAgentEffective_MinFreeDiskUnsetEverywhereIsDefaultOnePercent(t *testing.T) {
	got := effMinFree(t, "", "")
	require.False(t, got.Set, "unset stays unset so the caller can tell it was defaulted")
	require.Equal(t, DiskThreshold{Percent: 1}, got.OrDefault())
}

func TestAgentEffective_MinFreeDiskForms(t *testing.T) {
	cases := []struct {
		name, val string
		want      DiskThreshold
	}{
		{"legacy bytes", "5368709120", DiskThreshold{Bytes: 5368709120, Set: true}},
		{"percent", "1%", DiskThreshold{Percent: 1, Set: true}},
		{"size", "10GiB", DiskThreshold{Bytes: 10 << 30, Set: true}},
	}
	for _, c := range cases {
		t.Run(c.name+" env", func(t *testing.T) {
			require.Equal(t, c.want, effMinFree(t, c.val, ""))
		})
		t.Run(c.name+" yaml", func(t *testing.T) {
			require.Equal(t, c.want, effMinFree(t, "", "minFreeDisk: \""+c.val+"\"\n"))
		})
	}
	t.Run("legacy bytes as a yaml integer", func(t *testing.T) {
		require.Equal(t, DiskThreshold{Bytes: 5368709120, Set: true}, effMinFree(t, "", "minFreeDisk: 5368709120\n"))
	})
}

func TestAgentEffective_MinFreeDiskFileBeatsEnv(t *testing.T) {
	got := effMinFree(t, "10GiB", "minFreeDisk: 5%\n")
	require.Equal(t, DiskThreshold{Percent: 5, Set: true}, got)
}

func TestAgentEffective_MinFreeDiskEnvKeptWhenFileOmitsIt(t *testing.T) {
	got := effMinFree(t, "10GiB", "id: x\n")
	require.Equal(t, DiskThreshold{Bytes: 10 << 30, Set: true}, got)
}

func TestAgentEffective_ExplicitZeroDisablesAndBeatsDefault(t *testing.T) {
	t.Run("yaml", func(t *testing.T) {
		got := effMinFree(t, "", "minFreeDisk: 0\n")
		require.Equal(t, DiskThreshold{Set: true, Off: true}, got)
		require.Equal(t, got, got.OrDefault())
	})
	t.Run("env", func(t *testing.T) {
		got := effMinFree(t, "0", "")
		require.Equal(t, DiskThreshold{Set: true, Off: true}, got)
		require.Equal(t, got, got.OrDefault())
	})
	t.Run("yaml zero beats env value", func(t *testing.T) {
		require.True(t, effMinFree(t, "10GiB", "minFreeDisk: 0\n").Off)
	})
	t.Run("yaml value beats env zero", func(t *testing.T) {
		require.Equal(t, DiskThreshold{Percent: 2, Set: true}, effMinFree(t, "0", "minFreeDisk: 2%\n"))
	})
}

func TestAgentEffective_MalformedMinFreeDiskEnvFailsNamingTheVariable(t *testing.T) {
	for _, bad := range []string{"lots", "150%", "-5", "1.5"} {
		t.Setenv(minFreeEnv, bad)
		_, err := AgentEffective("")
		require.Error(t, err, bad)
		require.Contains(t, err.Error(), minFreeEnv, bad)
	}
}

func TestAgentEffective_MalformedMinFreeDiskYAMLFails(t *testing.T) {
	t.Setenv(minFreeEnv, "")
	_, err := AgentEffective(writeAgentYAML(t, "minFreeDisk: lots\n"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "minFreeDisk")
}
