// Package e2e is the acceptance harness (docs/spec/mvp.md §12): one test
// binary, two in-process Nodes with separate --dir trees, and a stock
// headscale in Docker via testcontainers. Build tag e2e; run with
//
//	go test -tags e2e ./e2e
package e2e
