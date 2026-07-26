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
agent_id=$(jq -er '.agentId | strings | select(length > 0 and test("^[^/\\\\]+$"))' "$legacy")
destination_dir="$HOME/.unified-cd/$agent_id"
(umask 077; mkdir -p "$destination_dir")
mv "$legacy" "$destination_dir/credential.json"
chmod 700 "$destination_dir"
chmod 600 "$destination_dir/credential.json"
```

### PowerShell

```powershell
$legacy = Join-Path $HOME '.unified-cd\credential.json'
$agentId = (Get-Content -Raw -LiteralPath $legacy | ConvertFrom-Json).agentId
if ([string]::IsNullOrWhiteSpace($agentId) -or $agentId -match '[\\/]') {
    throw 'credential .agentId must be one non-empty path component'
}
$destinationDir = Join-Path (Join-Path $HOME '.unified-cd') $agentId
New-Item -ItemType Directory -Force -Path $destinationDir | Out-Null
$owner = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
icacls $destinationDir /inheritance:r /grant:r "${owner}:(OI)(CI)F" | Out-Null
Move-Item -LiteralPath $legacy -Destination (Join-Path $destinationDir 'credential.json')
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
