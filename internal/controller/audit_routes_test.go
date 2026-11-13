package controller

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// auditExemptWriteRoutes lists human write routes in routeTable that are
// deliberately absent from auditActionTable, each with the reason. Adding a
// human write route to neither this set nor auditActionTable fails
// TestEveryHumanWriteRouteIsAuditedOrExempt.
//
// Public and agent rows (scopePublic, scopeAgent, scopeSelf) are intentionally
// not guarded: they have no human principal to attribute and are either
// audited by their own code paths (agent enrollment exchange and token
// refresh, gitcredential.token_issue) or are protocol endpoints (dex, device
// callback, webhook ingress, agent artifact upload, UI assets).
var auditExemptWriteRoutes = map[string]string{
	"POST /api/v1/authz/explain": "read-like dry-run query (global AuthzRead); changes no state",
}

func isWriteMethod(m string) bool {
	switch m {
	case mPost, mPut, "PATCH", mDelete, mAny:
		return true
	}
	return false
}

func TestEveryHumanWriteRouteIsAuditedOrExempt(t *testing.T) {
	for _, row := range routeTable {
		if !isHumanRow(row) || !isWriteMethod(row.Method) {
			continue
		}
		key := row.Method + " " + row.Pattern
		_, audited := auditActionTable[key]
		_, exempt := auditExemptWriteRoutes[key]
		require.Truef(t, audited || exempt,
			"%s is a write route with no audit action: add it to auditActionTable in audit.go, or to auditExemptWriteRoutes with a reason", key)
	}
}

func TestAuditTablesMatchRouteTable(t *testing.T) {
	rows := map[string]routeRule{}
	for _, row := range routeTable {
		rows[row.Method+" "+row.Pattern] = row
	}
	for key := range auditActionTable {
		row, ok := rows[key]
		require.Truef(t, ok, "auditActionTable entry %q has no routeTable row: remove the stale entry or fix the key", key)
		require.Truef(t, isHumanRow(row), "auditActionTable entry %q is not a human route", key)
	}
	for key, reason := range auditExemptWriteRoutes {
		row, ok := rows[key]
		require.Truef(t, ok, "auditExemptWriteRoutes entry %q has no routeTable row: remove it", key)
		require.Truef(t, isHumanRow(row) && isWriteMethod(row.Method), "auditExemptWriteRoutes entry %q is not a human write route: remove it", key)
		require.NotEmptyf(t, strings.TrimSpace(reason), "auditExemptWriteRoutes entry %q needs a reason", key)
		_, audited := auditActionTable[key]
		require.Falsef(t, audited, "%q is in both auditActionTable and auditExemptWriteRoutes: remove the exemption", key)
	}
}
