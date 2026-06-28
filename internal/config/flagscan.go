package config

import "strings"

// FindFlag scans args (typically os.Args[1:]) for a named flag and returns
// its value. Supports both "-name=value" / "--name=value" and
// "-name value" / "--name value" forms. Returns "" if not found.
func FindFlag(args []string, name string) string {
	prefixes := []string{"-" + name + "=", "--" + name + "="}
	shorts := []string{"-" + name, "--" + name}

	for i, arg := range args {
		for _, p := range prefixes {
			if strings.HasPrefix(arg, p) {
				return strings.TrimPrefix(arg, p)
			}
		}
		for _, s := range shorts {
			if arg == s && i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return ""
}
