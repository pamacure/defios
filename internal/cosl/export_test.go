package cosl

import "testing"

// WithoutType removes a type from the registry for the test, as a reader
// built before that type existed would see the log.
func WithoutType(t *testing.T, id uint32) {
	t.Helper()
	k, ok := registry[id]
	if !ok {
		t.Fatalf("type %d is not registered", id)
	}
	delete(registry, id)
	t.Cleanup(func() { registry[id] = k })
}
