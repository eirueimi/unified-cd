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

// VersionRequested reports whether args contains -version / --version.
//
// The controller and agent binaries pre-scan for it before loading their
// configuration so `--version` answers in an image that has no config file,
// DSN, or server URL — that is exactly the situation an operator is in when
// they need to find out which build an image tag actually contains.
// Everything after a bare "--" is a positional argument, not a flag.
func VersionRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "-version" || arg == "--version" {
			return true
		}
	}
	return false
}
