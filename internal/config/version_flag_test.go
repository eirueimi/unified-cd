package config

import "testing"

func TestVersionRequested(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"double dash", []string{"--version"}, true},
		{"single dash", []string{"-version"}, true},
		{"among other flags", []string{"-f", "cfg.yaml", "--version"}, true},
		{"absent", []string{"-f", "cfg.yaml"}, false},
		{"empty", nil, false},
		// A value that happens to read like the flag is not the flag.
		{"after terminator", []string{"--", "--version"}, false},
		{"prefix only", []string{"--versionfoo"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VersionRequested(tc.args); got != tc.want {
				t.Errorf("VersionRequested(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
