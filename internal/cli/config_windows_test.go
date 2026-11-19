//go:build windows

package cli

import (
	"fmt"
	"io/fs"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/windows"
)

func TestIsRenameBusy_Windows(t *testing.T) {
	for _, e := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_ACCESS_DENIED, fs.ErrPermission} {
		wrapped := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: e}
		assert.True(t, isRenameBusy(wrapped), "%v", e)
		assert.True(t, isRenameBusy(fmt.Errorf("wrap: %w", wrapped)), "%v", e)
	}
	assert.False(t, isRenameBusy(fs.ErrNotExist))
	assert.False(t, isRenameBusy(&os.LinkError{Op: "rename", Err: windows.ERROR_DISK_FULL}))
}
