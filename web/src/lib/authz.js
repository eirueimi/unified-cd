// Client-side mirror of internal/authz Grants.Can. The decision rule is shared
// with the server through authz-cases.json, which both test suites read; the
// server remains the authority and this only decides what to show enabled.

const has = (list, permission) => Array.isArray(list) && list.includes(permission);

// Proper ancestor folders of a qualified job name: "a/b/c" -> ["a", "a/b"].
// A job at the root has none. Matching is byte-exact (case-sensitive).
function ancestors(jobName) {
  const parts = String(jobName).split('/');
  const out = [];
  for (let i = 1; i < parts.length; i++) out.push(parts.slice(0, i).join('/'));
  return out;
}

function folderPerms(grants, folder) {
  const folders = grants.folders;
  return folders && Object.prototype.hasOwnProperty.call(folders, folder) ? folders[folder] : undefined;
}

// can reports whether grants (the permissions object of /auth/me) hold
// permission on jobName: global first, then every proper ancestor folder.
export function can(grants, permission, jobName) {
  if (!grants) return false;
  if (has(grants.global, permission)) return true;
  if (!jobName) return false;
  return ancestors(jobName).some((f) => has(folderPerms(grants, f), permission));
}

// canAnywhere reports whether permission is held globally or on any folder.
export function canAnywhere(grants, permission) {
  if (!grants) return false;
  if (has(grants.global, permission)) return true;
  return Object.values(grants.folders || {}).some((perms) => has(perms, permission));
}

// allows is the UI gate: while grants are unknown (null/undefined) it is
// permissive so the page renders as before and the server decides.
export function allows(grants, permission, jobName) {
  if (grants == null) return true;
  return jobName === undefined ? canAnywhere(grants, permission) : can(grants, permission, jobName);
}

// requiresText is the tooltip for a control disabled by allows().
export function requiresText(permission, jobName) {
  return jobName === undefined ? `requires ${permission}` : `requires ${permission} on ${jobName}`;
}
