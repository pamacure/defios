package vault

import "testing"

// SealAsInProduction makes Seal seal with WorkFactor, as outside a test
// binary, until t ends.
func SealAsInProduction(t *testing.T) {
	was := workFactor
	workFactor = WorkFactor
	t.Cleanup(func() { workFactor = was })
}
