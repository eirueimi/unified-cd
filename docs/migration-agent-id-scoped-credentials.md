# Migrating to ID-scoped agent credentials

Agent refresh credentials now use a directory named for the agent identity.
Stop the affected agent before moving its credential so it cannot refresh or
write the file while the migration is in progress.

| Before | After |
|---|---|
| The implicit default was `$HOME/.unified-cd/credential.json`. | The default is always `$HOME/.unified-cd/<agent-id>/credential.json`. |
| An ID-less startup implicitly used the shared credential. | An ID-less, token-less startup discovers exactly one ID-scoped credential. |
| Multiple agents could collide on the shared credential. | Multiple ID-scoped credentials are ambiguous until `--id` or `--credential-file` selects one. |
| A legacy shared credential could be used implicitly. | The legacy shared file is ignored unless it is explicitly passed with `--credential-file`. |

## Expected startup error

If an ID-less, token-less startup finds more than one ID-scoped default
credential, it exits with this exact error:

```
multiple default agent credential files found; set --id or --credential-file
```

Restart it with `--id <agent-id>` or `--credential-file <path>` to make the
selection explicit. A valid explicit enrollment token takes precedence over
local discovery and saves the refreshed credential under the returned ID.

## Move a legacy credential

The following commands extract only `.agentId`; they never print
`.refreshToken`. Stop the affected agent first, then run the commands as the
same OS account that owns the credential.

### POSIX shell

This requires `jq`.

```bash
legacy="$HOME/.unified-cd/credential.json"
root="$HOME/.unified-cd"
if ! agent_id=$(jq -er '.agentId | strings | select(test("^[a-z0-9]([a-z0-9._-]*[a-z0-9])?$"))' "$legacy"); then
  echo "credential .agentId is not a portable canonical agent ID" >&2
  exit 1
fi
case "${agent_id%%.*}" in
  con|prn|aux|nul|com[1-9]|lpt[1-9])
    echo "credential .agentId uses a reserved Windows name" >&2
    exit 1
    ;;
esac
destination_dir="$root/$agent_id"
destination_file="$destination_dir/credential.json"

[ -d "$root" ] && [ ! -L "$root" ] || {
  echo "credential root must be a real directory" >&2
  exit 1
}
if [ -e "$destination_dir" ] || [ -L "$destination_dir" ]; then
  echo "destination credential directory already exists" >&2
  exit 1
fi
if [ -e "$destination_file" ] || [ -L "$destination_file" ]; then
  echo "destination credential already exists" >&2
  exit 1
fi
if ! (umask 077; mkdir "$destination_dir"); then
  echo "could not create destination credential directory" >&2
  exit 1
fi
[ -d "$destination_dir" ] && [ ! -L "$destination_dir" ] || {
  echo "destination credential directory is redirected" >&2
  exit 1
}
if [ -e "$destination_file" ] || [ -L "$destination_file" ]; then
  echo "destination credential already exists" >&2
  exit 1
fi
if ! mv -n "$legacy" "$destination_file"; then
  echo "credential move failed" >&2
  exit 1
fi
if [ -e "$legacy" ]; then
  echo "credential move did not complete; destination may have appeared concurrently" >&2
  exit 1
fi
chmod 700 "$destination_dir"
chmod 600 "$destination_file"
```

### PowerShell

```powershell
$legacy = Join-Path $HOME '.unified-cd\credential.json'
$agentId = (Get-Content -Raw -LiteralPath $legacy | ConvertFrom-Json).agentId
if ($agentId -cnotmatch '^[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$') {
    throw "credential .agentId is not a portable canonical agent ID"
}
$reservedBase = ($agentId -split '\.', 2)[0]
if ($reservedBase -in @('con', 'prn', 'aux', 'nul') -or $reservedBase -match '^(com|lpt)[1-9]$') {
    throw "credential .agentId uses a reserved Windows name"
}
$root = Join-Path $HOME '.unified-cd'
$rootItem = Get-Item -Force -LiteralPath $root
if (($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'credential root must not be a reparse point'
}
$destinationDir = Join-Path $root $agentId
if (Test-Path -LiteralPath $destinationDir) {
    throw 'destination credential directory already exists'
}
New-Item -ItemType Directory -Path $destinationDir | Out-Null
$destinationItem = Get-Item -Force -LiteralPath $destinationDir
if (($destinationItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'destination credential directory must not be a reparse point'
}
$destinationFile = Join-Path $destinationDir 'credential.json'
if (Test-Path -LiteralPath $destinationFile) {
    throw 'destination credential already exists'
}
$currentUser = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$directorySecurity = [System.Security.AccessControl.DirectorySecurity]::new()
$directorySecurity.SetAccessRuleProtection($true, $false)
$directorySecurity.SetOwner($currentUser.User)
$ownerRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
    $currentUser.User,
    [System.Security.AccessControl.FileSystemRights]::FullControl,
    ([System.Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [System.Security.AccessControl.InheritanceFlags]::ObjectInherit),
    [System.Security.AccessControl.PropagationFlags]::None,
    [System.Security.AccessControl.AccessControlType]::Allow
)
$directorySecurity.AddAccessRule($ownerRule)
Set-Acl -LiteralPath $destinationDir -AclObject $directorySecurity
Move-Item -LiteralPath $legacy -Destination $destinationFile
```

The commands deliberately do not enumerate or print credential contents. Do
not move another agent's ID-scoped credential: an agent process only writes
its own effective ID's default path and never removes another ID's credential.

## Roll back or retain the legacy path temporarily

To use a legacy shared file without moving it yet, select it explicitly:

```bash
unified-cd-agent --credential-file "$HOME/.unified-cd/credential.json"
```

After moving the credential, roll back by stopping the agent, moving the file
back to the shared location, and starting it with the same explicit
`--credential-file` path. Do not rely on implicit discovery for the legacy
path; it is intentionally ignored.
