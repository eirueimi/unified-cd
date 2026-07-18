package secrets

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// Binding identifies what a ciphertext belongs to. Its canonical encoding is
// mixed into AES-GCM as additional authenticated data, so a ciphertext copied
// into a different row fails to decrypt instead of silently yielding another
// secret's plaintext.
//
// Fields are unexported and the type is constructed only through the
// constructors below, so an empty or partially-populated Binding cannot be
// built by accident.
type Binding struct {
	kind   string
	fields []string
}

// SecretBinding binds a ciphertext to a row in the secrets table.
func SecretBinding(name, scope, scopeRef string) Binding {
	return Binding{kind: "secret", fields: []string{name, scope, scopeRef}}
}

// SessionRefreshBinding binds an encrypted OIDC refresh token to its session.
func SessionRefreshBinding(sessionID string) Binding {
	return Binding{kind: "session_refresh", fields: []string{sessionID}}
}

// canonical returns a length-prefixed encoding of the binding.
//
// Length prefixes are mandatory, not stylistic: with plain concatenation
// ("a","bc") and ("ab","c") encode to identical bytes, which would let one
// secret's ciphertext be substituted for another's — exactly what the AAD
// exists to prevent.
func (b Binding) canonical() []byte {
	out := make([]byte, 0, 64)
	out = appendField(out, b.kind)
	for _, f := range b.fields {
		out = appendField(out, f)
	}
	return out
}

func appendField(dst []byte, s string) []byte {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	dst = append(dst, n[:]...)
	return append(dst, s...)
}

// String returns a human-readable identifier for logs and error messages.
// It contains only identifying fields, never a secret value.
func (b Binding) String() string {
	return fmt.Sprintf("%s(%s)", b.kind, strings.Join(b.fields, ","))
}
