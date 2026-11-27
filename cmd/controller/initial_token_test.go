package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store/storetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareInitialTokenRace(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	dirs := []string{t.TempDir(), t.TempDir()}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = prepareInitialToken(t.Context(), pg, false, "", dirs)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	var files []string
	for _, d := range dirs {
		p := filepath.Join(d, controller.InitialTokenFileName)
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	require.Len(t, files, 1)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(files[0])
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	b, err := os.ReadFile(files[0])
	require.NoError(t, err)
	value := string(b)
	assert.True(t, strings.HasPrefix(value, "uci_"))
	assert.Len(t, value, len("uci_")+40)

	state, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, controller.HashToken(value), state.InitialTokenHash)
}

func TestPrepareInitialTokenSkipsWithSSO(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	dir := t.TempDir()
	path, err := prepareInitialToken(t.Context(), pg, true, "", []string{dir})
	require.NoError(t, err)
	assert.Empty(t, path)
	_, statErr := os.Stat(filepath.Join(dir, controller.InitialTokenFileName))
	assert.True(t, os.IsNotExist(statErr))
	state, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.Empty(t, state.InitialTokenHash)
}

func TestPrepareInitialTokenProvided(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	dir := t.TempDir()
	path, err := prepareInitialToken(t.Context(), pg, true, "uci_provided", []string{dir})
	require.NoError(t, err)
	assert.Empty(t, path)
	_, statErr := os.Stat(filepath.Join(dir, controller.InitialTokenFileName))
	assert.True(t, os.IsNotExist(statErr))
	state, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, controller.HashToken("uci_provided"), state.InitialTokenHash)

	// A second, different value does not replace the stored hash.
	_, err = prepareInitialToken(t.Context(), pg, true, "uci_other", []string{dir})
	require.NoError(t, err)
	state, err = pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, controller.HashToken("uci_provided"), state.InitialTokenHash)
}

func TestPrepareInitialTokenNoDir(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	_, err := prepareInitialToken(t.Context(), pg, false, "", []string{missing, ""})
	require.ErrorIs(t, err, errNoWritableDir)
	_, err = prepareInitialToken(t.Context(), pg, false, "", nil)
	require.ErrorIs(t, err, errNoWritableDir)
	// Nothing was claimed, so a later start with a writable directory works.
	state, err := pg.GetBootstrapState(t.Context())
	require.NoError(t, err)
	assert.Empty(t, state.InitialTokenHash)
}

func TestPrepareInitialTokenRemovesUsedFile(t *testing.T) {
	pg := storetest.NewTestPostgres(t)
	dir := t.TempDir()
	path := filepath.Join(dir, controller.InitialTokenFileName)
	require.NoError(t, os.WriteFile(path, []byte("uci_used"), 0o600))
	ok, err := pg.ClaimInitialToken(t.Context(), controller.HashToken("uci_used"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = pg.ConsumeInitialToken(t.Context(), controller.HashToken("uci_used"))
	require.NoError(t, err)
	require.True(t, ok)

	_, err = prepareInitialToken(t.Context(), pg, false, "", []string{dir})
	require.NoError(t, err)
	_, statErr := os.Stat(path)
	assert.True(t, os.IsNotExist(statErr))
}

func TestLegacyTokenKeysAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.yaml")
	require.NoError(t, os.WriteFile(path, []byte("token: old-secret\nallowDefaultToken: true\n"), 0o600))
	eff, err := config.ControllerEffective(path)
	require.NoError(t, err)
	assert.Equal(t, "old-secret", eff.Token)
}
