// Package authz is the dependency-free core of matrix authorization: the
// permission catalogue, role bundles, folder normalisation, and the grant sets
// that answer "may this principal do P on job J".
package authz

// Permission is one dotted permission name from the catalogue.
type Permission string

// Folder-scopable permissions (catalogue positions 0-8).
const (
	JobRead           Permission = "job.read"
	JobConfigure      Permission = "job.configure"
	JobDelete         Permission = "job.delete"
	RunTrigger        Permission = "run.trigger"
	RunReplay         Permission = "run.replay"
	RunCancel         Permission = "run.cancel"
	RunDelete         Permission = "run.delete"
	RunApprove        Permission = "run.approve"
	ScheduleConfigure Permission = "schedule.configure"
)

// Global-only permissions (catalogue positions 9-24).
const (
	SecretList         Permission = "secret.list"
	SecretWrite        Permission = "secret.write"
	GitCredentialRead  Permission = "gitcredential.read"
	GitCredentialWrite Permission = "gitcredential.write"
	VarsRead           Permission = "vars.read"
	VarsWrite          Permission = "vars.write"
	WebhookRead        Permission = "webhook.read"
	WebhookWrite       Permission = "webhook.write"
	AppSourceRead      Permission = "appsource.read"
	AppSourceWrite     Permission = "appsource.write"
	AgentRead          Permission = "agent.read"
	AgentManage        Permission = "agent.manage"
	TokenCreate        Permission = "token.create"
	TokenManage        Permission = "token.manage"
	AuditRead          Permission = "audit.read"
	AuthzRead          Permission = "authz.read"
)

// folderScopableCount is the number of leading catalogue entries that may be
// granted on a folder.
const folderScopableCount = 9

var catalogue = []Permission{
	JobRead, JobConfigure, JobDelete,
	RunTrigger, RunReplay, RunCancel, RunDelete, RunApprove,
	ScheduleConfigure,
	SecretList, SecretWrite,
	GitCredentialRead, GitCredentialWrite,
	VarsRead, VarsWrite,
	WebhookRead, WebhookWrite,
	AppSourceRead, AppSourceWrite,
	AgentRead, AgentManage,
	TokenCreate, TokenManage,
	AuditRead, AuthzRead,
}

// bitOf returns the Set bit index of p, or -1 when p is not in the catalogue.
func bitOf(p Permission) int {
	for i, c := range catalogue {
		if c == p {
			return i
		}
	}
	return -1
}

// All returns the catalogue in order: the folder-scopable permissions first,
// then the global-only ones. The result is a copy.
func All() []Permission {
	return append([]Permission(nil), catalogue...)
}

// Parse resolves a dotted permission name.
func Parse(name string) (Permission, bool) {
	p := Permission(name)
	return p, bitOf(p) >= 0
}

// FolderScopable reports whether p may be granted on a folder.
func FolderScopable(p Permission) bool {
	i := bitOf(p)
	return i >= 0 && i < folderScopableCount
}

// RoleNames lists the built-in roles, lowest privilege last.
var RoleNames = []string{"admin", "developer", "viewer"}

// Bundle returns the global permission set of a built-in role.
func Bundle(role string) (Set, bool) {
	viewer := Set(0).With(JobRead, VarsRead, WebhookRead, AppSourceRead, AgentRead)
	switch role {
	case "viewer":
		return viewer, true
	case "developer":
		return viewer.With(JobConfigure, JobDelete, RunTrigger, RunReplay, RunCancel,
			RunDelete, RunApprove, ScheduleConfigure, SecretList, TokenCreate), true
	case "admin":
		return AllSet(), true
	}
	return 0, false
}
