package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"syscall"
	"time"

	"github.com/eirueimi/unified-cd/internal/authz"
	"github.com/eirueimi/unified-cd/internal/config"
	"github.com/eirueimi/unified-cd/internal/controller"
	"github.com/eirueimi/unified-cd/internal/store"
)

const (
	// recoveryTTLDefault and recoveryTTLMax bound the lifetime of a recovery
	// PAT.
	recoveryTTLDefault = time.Hour
	recoveryTTLMax     = 24 * time.Hour

	// recoveryActor is the audit actor (name and id) and the PAT creator of
	// the recovery command.
	recoveryActor = "recovery"

	adminUsage = "usage: <controller binary> admin recover [-f config] [--dsn dsn] [--ttl 1h] [--enable-admin]"
)

// runAdmin runs a `<controller binary> admin` subcommand and returns the process
// exit code. The token goes to stdout, everything else to stderr.
func runAdmin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "recover" {
		_, _ = fmt.Fprintln(stderr, adminUsage)
		return 2
	}
	return runRecover(args[1:], stdout, stderr)
}

// runRecover mints a short-lived PAT of the built-in admin account straight
// in the database (spec 6.6). Reaching the database with the controller's
// configuration is the only gate. It never starts the HTTP server or any
// background loop.
func runRecover(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("admin recover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configFile := fs.String("f", config.FindFlag(args, "f"), "config file path (YAML)")
	dsnFlag := fs.String("dsn", "", "postgres DSN (env: UNIFIED_DB_DSN)")
	ttl := fs.Duration("ttl", recoveryTTLDefault, "lifetime of the recovery PAT (maximum 24h)")
	enableAdmin := fs.Bool("enable-admin", false, "enable the built-in admin account until the PAT expires, even if the configuration disables it")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintln(stderr, adminUsage)
		return 2
	}
	if *ttl <= 0 {
		_, _ = fmt.Fprintln(stderr, "--ttl must be positive")
		return 2
	}
	if *ttl > recoveryTTLMax {
		_, _ = fmt.Fprintln(stderr, "--ttl must not exceed 24h")
		return 2
	}

	eff, err := config.ControllerEffective(*configFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "config file: %v\n", err)
		return 1
	}
	dsn := eff.DSN
	if *dsnFlag != "" {
		dsn = *dsnFlag
	}
	if dsn == "" {
		_, _ = fmt.Fprintln(stderr, "dsn is required (--dsn, UNIFIED_DB_DSN, or config file)")
		return 1
	}
	if !eff.AdminAccountEnabled() && !*enableAdmin {
		_, _ = fmt.Fprintln(stderr, "the built-in admin account is disabled in the configuration; pass --enable-admin")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	poolCfg, _ := postgresPoolConfigFromEnv()
	pg, err := store.NewPostgresWithPoolConfig(ctx, dsn, poolCfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "store init: %v\n", err)
		return 1
	}
	defer pg.Close()
	if err := pg.Migrate(dsn); err != nil {
		_, _ = fmt.Fprintf(stderr, "migrate: %v\n", err)
		return 1
	}

	token, expires, err := recoverAdminPAT(ctx, pg, time.Now().UTC(), *ttl, *enableAdmin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "recover: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, token)
	_, _ = fmt.Fprintf(stderr, "recovery token printed on stdout; it expires at %s\n", expires.Format(time.RFC3339))
	return 0
}

// recoverAdminPAT creates the recovery PAT of the built-in admin account,
// extends the admin enablement lease when enableAdmin is set, and writes the
// audit row. It returns the token value and its expiry. Nothing is returned
// to print unless every step succeeded.
func recoverAdminPAT(ctx context.Context, st store.Store, now time.Time, ttl time.Duration, enableAdmin bool) (string, time.Time, error) {
	accounts, err := st.ListAccounts(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	var adminID string
	for _, a := range accounts {
		if a.Kind == store.AccountKindService && a.Name == authz.BuiltinAdminAccount {
			adminID = a.ID
			break
		}
	}
	if adminID == "" {
		return "", time.Time{}, errors.New("the built-in admin account does not exist")
	}

	token, err := controller.NewPATToken()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate token: %w", err)
	}
	expires := now.Add(ttl)
	name := "recovery-" + now.Format("20060102T150405Z")
	pat, err := st.CreatePAT(ctx, store.NewPAT{
		Name: name, TokenHash: controller.HashToken(token), AccountID: adminID,
		ExpiresAt: &expires, CreatedBy: recoveryActor,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	// Side effects are ordered PAT, audit row, lease. The lease comes last so a
	// failed run never leaves a config-disabled admin account enabled without an
	// audit trail. On any failure after the PAT exists it is deleted (best
	// effort), so a failed run leaves neither a usable PAT nor a lease.
	revoke := func() {
		if _, derr := st.DeleteAccountPAT(context.WithoutCancel(ctx), pat.ID, adminID); derr != nil {
			slog.Warn("recover: could not delete the unused recovery PAT", "patId", pat.ID, "error", derr)
		}
	}
	if err := st.InsertAuditLog(ctx, store.AuditEntry{
		Actor: recoveryActor, ActorID: recoveryActor,
		Method: "CLI", Path: "admin recover",
		Action: "admin.recover", Resource: name, Status: 200,
		Details: map[string]any{"patId": pat.ID, "ttl": ttl.String(), "enableAdmin": enableAdmin},
	}); err != nil {
		revoke()
		return "", time.Time{}, fmt.Errorf("write audit log: %w", err)
	}
	if enableAdmin {
		if err := st.SetAdminEnabledUntil(ctx, expires); err != nil {
			revoke()
			return "", time.Time{}, err
		}
	}
	return token, expires, nil
}
