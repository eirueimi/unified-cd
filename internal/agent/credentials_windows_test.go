//go:build windows

package agent

import (
	"testing"

	"golang.org/x/sys/windows"

	"github.com/stretchr/testify/require"
)

func TestRejectCredentialReparseAttributes(t *testing.T) {
	require.NoError(t, rejectCredentialReparseAttributes(windows.FILE_ATTRIBUTE_DIRECTORY))
	require.EqualError(
		t,
		rejectCredentialReparseAttributes(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT),
		"credential directory must not be a symbolic link or reparse point",
	)
}
