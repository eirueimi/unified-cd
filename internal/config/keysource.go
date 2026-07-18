package config

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/eirueimi/unified-cd/internal/secrets"
)

// supportedKMSSchemes lists the URI schemes the configuration surface accepts.
// None is implemented yet; the surface exists so the follow-up change that adds
// a provider does not have to redefine configuration.
var supportedKMSSchemes = []string{"hashivault"}

// KeySource describes where the controller's key-encryption key comes from.
// Exactly one of KeyFile or KMSURI may be set; DevMode is an explicit opt-in
// used only when neither is.
type KeySource struct {
	KeyFile string
	KMSURI  string
	DevMode bool
}

// Validate rejects ambiguous configuration. Which key is in effect must never
// be a guess, so supplying two sources is an error rather than a precedence
// rule.
func (k KeySource) Validate() error {
	if k.KeyFile != "" && k.KMSURI != "" {
		return fmt.Errorf("set exactly one of UNIFIED_CONTROLLER_KEY_FILE or UNIFIED_KMS_URI, not both")
	}
	return nil
}

// Resolved is the outcome of resolving a KeySource.
//
// Warnings are returned rather than logged because this package does no
// logging: slog is not configured until main.go has parsed flags. This
// mirrors the existing matrixMaxEnvWarning pattern in cmd/controller/main.go,
// which collects a warning during flag registration and logs it once the
// logger exists.
type Resolved struct {
	KeyManager secrets.KeyManager
	// Description names the key's origin, for the startup log.
	Description string
	// Warnings are operator-facing messages main.go should emit via slog.Warn.
	Warnings []string

	// closeFn releases whatever the key source acquired. A local or ephemeral
	// key acquires nothing, so it stays nil and Close is a no-op.
	closeFn func() error
}

// Close releases resources held by the key manager — for a KMS-backed source,
// the background token-renewal loop. It is safe to call on a source that
// acquired nothing, and safe to call more than once.
func (r *Resolved) Close() error {
	if r.closeFn == nil {
		return nil
	}
	fn := r.closeFn
	r.closeFn = nil
	return fn()
}

// Resolve turns the configured source into a KeyManager plus any warnings.
func (k KeySource) Resolve(ctx context.Context) (Resolved, error) {
	if err := k.Validate(); err != nil {
		return Resolved{}, err
	}
	switch {
	case k.KMSURI != "":
		return Resolved{}, kmsError(k.KMSURI)
	case k.KeyFile != "":
		km, warnings, err := localFromFile(k.KeyFile)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{KeyManager: km, Description: "key file " + k.KeyFile, Warnings: warnings}, nil
	case k.DevMode:
		km, err := secrets.NewLocalKeyManager(hex.EncodeToString(secrets.GenerateKey()))
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{
			KeyManager:  km,
			Description: "ephemeral development key",
			Warnings: []string{"UNIFIED_DEV_MODE is set — using an ephemeral encryption key. " +
				"Secrets stored now cannot be decrypted after a restart. Never use this in production."},
		}, nil
	default:
		return Resolved{}, fmt.Errorf(
			"no encryption key configured: run `unified-cli keygen --out /etc/unified-cd/kek` " +
				"and set UNIFIED_CONTROLLER_KEY_FILE to that path " +
				"(or set UNIFIED_KMS_URI, or UNIFIED_DEV_MODE=1 for a throwaway key)")
	}
}

func kmsError(uri string) error {
	scheme, _, found := strings.Cut(uri, "://")
	if !found || scheme == "" {
		return fmt.Errorf("UNIFIED_KMS_URI %q is malformed; expected <scheme>://<key>, where scheme is one of: %s",
			uri, strings.Join(supportedKMSSchemes, ", "))
	}
	for _, s := range supportedKMSSchemes {
		if s == scheme {
			return fmt.Errorf("UNIFIED_KMS_URI scheme %q is not implemented in this build; "+
				"use UNIFIED_CONTROLLER_KEY_FILE for now", scheme)
		}
	}
	return fmt.Errorf("UNIFIED_KMS_URI scheme %q is not supported; supported schemes: %s",
		scheme, strings.Join(supportedKMSSchemes, ", "))
}

func localFromFile(path string) (secrets.KeyManager, []string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	var warnings []string
	// File modes are not meaningful on Windows, so the check is skipped there.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf(
			"key file %s is readable by group or others (mode %#o); restrict it with chmod 600",
			path, info.Mode().Perm()))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	hexKey := strings.TrimSpace(string(raw))
	if len(hexKey) != 64 {
		return nil, nil, fmt.Errorf("key file %s must contain 64 hex characters, got %d", path, len(hexKey))
	}
	km, err := secrets.NewLocalKeyManager(hexKey)
	if err != nil {
		return nil, nil, fmt.Errorf("key file %s: %w", path, err)
	}
	return km, warnings, nil
}
