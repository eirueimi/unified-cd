package cli

import "testing"

func TestResolveConfig_EnvOverridesConfigFile(t *testing.T) {
	fileCfg := Config{Server: "https://from-config-file.example.com", Token: "file-token"}
	got := resolveConfig(fileCfg, "https://from-env.example.com", "env-token", "", "")

	if got.Server != "https://from-env.example.com" {
		t.Errorf("expected env server to override config file, got %q", got.Server)
	}
	if got.Token != "env-token" {
		t.Errorf("expected env token to override config file, got %q", got.Token)
	}
}

func TestResolveConfig_FlagOverridesEnv(t *testing.T) {
	fileCfg := Config{Server: "https://from-config-file.example.com", Token: "file-token"}
	got := resolveConfig(fileCfg, "https://from-env.example.com", "env-token", "https://from-flag.example.com", "flag-token")

	if got.Server != "https://from-flag.example.com" {
		t.Errorf("expected flag server to override env, got %q", got.Server)
	}
	if got.Token != "flag-token" {
		t.Errorf("expected flag token to override env, got %q", got.Token)
	}
}

func TestResolveConfig_FallsBackToConfigFileWhenNoOverrides(t *testing.T) {
	fileCfg := Config{Server: "https://from-config-file.example.com", Token: "file-token"}
	got := resolveConfig(fileCfg, "", "", "", "")

	if got.Server != fileCfg.Server {
		t.Errorf("expected config file server to remain, got %q", got.Server)
	}
	if got.Token != fileCfg.Token {
		t.Errorf("expected config file token to remain, got %q", got.Token)
	}
}
