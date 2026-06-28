package e2e

import (
	"bytes"
	"io"
)

func asReader(b []byte) io.Reader { return bytes.NewReader(b) }
