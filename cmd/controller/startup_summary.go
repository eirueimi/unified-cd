package main

import "log/slog"

// capabilityState is one optional capability's resolved state at startup,
// together with what an operator loses when it is off.
//
// The controller initializes each subsystem with its own log line, so a
// capability that quietly switched itself off is visible only as the absence
// of a line among twenty. Worse, some settings report themselves as enabled
// while being inert — see the logTrim case below. The summary exists so a
// misconfiguration is legible in one record instead of inferred from a feature
// that never fires.
type capabilityState struct {
	// Name is the log attribute key, e.g. "objectStore".
	Name string
	// State is the resolved value, e.g. "s3", "local", "none".
	State string
	// Lost is empty when the capability is fully available. Otherwise it names
	// what does not work and the setting that restores it.
	Lost string
}

// startupInputs is the resolved configuration the summary reports on.
type startupInputs struct {
	// ObjectStore is "s3", "local", or "none", matching the selection order in
	// main (S3, then UNIFIED_DATA_DIR, then nothing).
	ObjectStore string
	// KeyDesc is config.Resolved.Description — the key's origin.
	KeyDesc string
	// KeyEphemeral is config.Resolved.Ephemeral: the key does not survive a
	// restart, so neither do the secrets encrypted with it.
	KeyEphemeral bool
	OIDC         bool
	WebUI        bool
	LogTrimDays  int
}

func summarizeStartup(in startupInputs) []capabilityState {
	caps := []capabilityState{
		{Name: "objectStore", State: in.ObjectStore},
		{Name: "secretKey", State: in.KeyDesc},
		{Name: "sso", State: onOff(in.OIDC, "oidc")},
		{Name: "webUI", State: onOff(in.WebUI, "served")},
	}

	if in.ObjectStore == "none" {
		caps[0].Lost = "log archival and artifacts are disabled; set UNIFIED_S3_ENDPOINT and UNIFIED_S3_BUCKET, or UNIFIED_DATA_DIR for development"
	}
	if in.KeyEphemeral {
		caps[1].Lost = "the encryption key is ephemeral: every stored secret is unreadable after a restart; set UNIFIED_CONTROLLER_KEY_FILE or UNIFIED_KMS_URI"
	}
	if !in.OIDC {
		caps[2].Lost = "SSO and the CLI device flow are unavailable; set UNIFIED_OIDC_ISSUER"
	}
	if !in.WebUI {
		caps[3].Lost = "/ui/* returns 404; set UNIFIED_WEB_DIR"
	}

	// Only reported when it contradicts itself. RunLogTrim returns immediately
	// with a nil object store, so no logs are lost — but startup otherwise
	// prints "log trim enabled" for a sweeper that will never run.
	if in.LogTrimDays > 0 && in.ObjectStore == "none" {
		caps = append(caps, capabilityState{
			Name:  "logTrim",
			State: "inert",
			Lost:  "log trim is configured but never runs without an object store; configure one, or unset UNIFIED_LOG_TRIM_DAYS",
		})
	}

	return caps
}

func onOff(on bool, whenOn string) string {
	if on {
		return whenOn
	}
	return "off"
}

// logStartupSummary emits one info record carrying every capability's state,
// then one warn record per degraded capability.
func logStartupSummary(in startupInputs) {
	caps := summarizeStartup(in)

	attrs := make([]any, 0, len(caps)*2)
	for _, c := range caps {
		attrs = append(attrs, c.Name, c.State)
	}
	slog.Info("startup summary", attrs...)

	for _, c := range caps {
		if c.Lost != "" {
			slog.Warn("degraded capability", "capability", c.Name, "state", c.State, "impact", c.Lost)
		}
	}
}
