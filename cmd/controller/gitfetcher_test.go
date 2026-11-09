package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGitTimeoutDefault(t *testing.T) {
	cases := map[string]time.Duration{
		"":      5 * time.Minute,
		"90s":   90 * time.Second,
		"0":     5 * time.Minute,
		"-1m":   5 * time.Minute,
		"bogus": 5 * time.Minute,
	}
	for in, want := range cases {
		t.Setenv("UNIFIED_GIT_TIMEOUT", in)
		if got := gitTimeoutDefault(); got != want {
			t.Errorf("UNIFIED_GIT_TIMEOUT=%q: got %s, want %s", in, got, want)
		}
	}
}

func TestGitMaxRepoBytesDefault(t *testing.T) {
	cases := map[string]int64{
		"":      1 << 30,
		"0":     0,
		"1024":  1024,
		"-5":    1 << 30,
		"bogus": 1 << 30,
	}
	for in, want := range cases {
		t.Setenv("UNIFIED_GIT_MAX_REPO_BYTES", in)
		if got := gitMaxRepoBytesDefault(); got != want {
			t.Errorf("UNIFIED_GIT_MAX_REPO_BYTES=%q: got %d, want %d", in, got, want)
		}
	}
}

func TestGitKnownHostsPath(t *testing.T) {
	if got := gitKnownHostsPath("/x/kh", "/data"); got != "/x/kh" {
		t.Errorf("explicit path must win, got %q", got)
	}
	if got := gitKnownHostsPath("", "/data"); got != filepath.Join("/data", ".ssh", "known_hosts") {
		t.Errorf("data-dir default wrong: %q", got)
	}
}
