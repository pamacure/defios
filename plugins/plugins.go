// Package plugins holds DeFiOS's built-in Plugins, one directory each in the
// layout of an installed one, compiled into the binary (ADR 0030): defios,
// and one per Network type (ADR 0042).
package plugins

import "embed"

// FS holds each built-in Plugin at its name. A new one is added to the
// embed line.
//
//go:embed defios bitcoin ethereum solana
var FS embed.FS
