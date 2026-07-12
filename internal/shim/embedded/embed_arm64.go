//go:build arm64

package embedded

import _ "embed"

// payload is the embedded linux/arm64 ucd-sh binary. See the package doc
// comment in embed.go for the placeholder contract and arch-selection
// rationale.
//
//go:embed ucd-sh-arm64
var payload []byte
