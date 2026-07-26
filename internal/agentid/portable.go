// Package agentid validates agent identities used as portable credential
// directory names.
package agentid

import (
	"errors"
	"strings"
)

// PortableError is returned when an agent ID cannot be used injectively as one
// path component across supported filesystems.
const PortableError = "agent ID must use lowercase ASCII letters, digits, '.', '_', or '-', start and end with a letter or digit, and not use a reserved Windows name"

// ValidatePortable accepts the canonical syntax for host-agent IDs that are
// used literally as credential directory names.
func ValidatePortable(id string) error {
	if id == "" || !isASCIIAlphaNumeric(id[0]) || !isASCIIAlphaNumeric(id[len(id)-1]) {
		return errors.New(PortableError)
	}
	for i := 0; i < len(id); i++ {
		if isASCIIAlphaNumeric(id[i]) || id[i] == '.' || id[i] == '_' || id[i] == '-' {
			continue
		}
		return errors.New(PortableError)
	}
	base, _, _ := strings.Cut(id, ".")
	if isWindowsReservedName(base) {
		return errors.New(PortableError)
	}
	return nil
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9'
}

func isWindowsReservedName(value string) bool {
	switch value {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(value) == 4 &&
		(value[:3] == "com" || value[:3] == "lpt") &&
		value[3] >= '1' && value[3] <= '9'
}
