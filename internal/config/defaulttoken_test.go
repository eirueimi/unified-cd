package config

import (
	"strings"
	"testing"
)

func TestCheckAdminToken(t *testing.T) {
	cases := []struct {
		name    string
		token   string
		dev     bool
		allow   bool
		wantErr bool
	}{
		{"real token", "s3cr3t-value-123", false, false, false},
		{"empty token is not this check's concern", "", false, false, false},
		{"default token refused", DefaultAdminToken, false, false, true},
		{"default token allowed in dev mode", DefaultAdminToken, true, false, false},
		{"default token allowed with opt-in", DefaultAdminToken, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckAdminToken(tc.token, tc.dev, tc.allow)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckAdminToken err=%v, wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				for _, want := range []string{"UNIFIED_TOKEN", "UNIFIED_ALLOW_DEFAULT_TOKEN", "UNIFIED_DEV_MODE"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %s", err, want)
					}
				}
			}
		})
	}
}
