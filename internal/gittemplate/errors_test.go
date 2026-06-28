package gittemplate

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsResolveError_True(t *testing.T) {
	err := newResolveError("bad yaml: %v", "boom")
	assert.True(t, IsResolveError(err))
}

func TestIsResolveError_WrappedStillTrue(t *testing.T) {
	err := newResolveError("cycle detected: %s", "git://x@v1")
	wrapped := fmt.Errorf("step %q: %w", "fetch", err)
	assert.True(t, IsResolveError(wrapped))
}

func TestIsResolveError_FalseForPlainError(t *testing.T) {
	assert.False(t, IsResolveError(errors.New("network unreachable")))
}

func TestIsResolveError_FalseForNil(t *testing.T) {
	assert.False(t, IsResolveError(nil))
}

func TestWrapResolveError_PreservesMessage(t *testing.T) {
	inner := errors.New("yaml: line 3: bad indentation")
	err := wrapResolveError(inner)
	assert.True(t, IsResolveError(err))
	assert.Contains(t, err.Error(), "bad indentation")
}
