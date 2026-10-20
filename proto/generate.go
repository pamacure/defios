// Package proto holds the protobuf schemas and their generated Go code.
//
// Regenerate with `go generate ./proto/...` (needs protoc and protoc-gen-go).
package proto

//go:generate protoc --go_out=. --go_opt=paths=source_relative vault/v1/vault.proto
