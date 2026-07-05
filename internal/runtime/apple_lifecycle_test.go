// internal/runtime/apple_lifecycle_test.go
package runtime

import "testing"

func TestAppleSatisfiesInterface(t *testing.T) {
	var _ ContainerRuntime = &appleContainer{}
}
