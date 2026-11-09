# GitCredential

## GitCredential

Stores Git authentication credentials for private repositories, used with `git://` template URIs and AppSource.

```yaml
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: <string>                  # required
  annotations:                    # optional
    <key>: <value>
spec:
  host: <string>                  # required — hostname to apply credentials to
  type: token | sshKey            # required
  secretRef: <string>             # required — name of StoredSecret containing the credential
  knownHosts: <string>            # optional, sshKey only — OpenSSH known_hosts lines pinning the server host key
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique credential name |
| `metadata.annotations` | map[string]string | No | Arbitrary key/value metadata |
| `spec.host` | string | Yes | Hostname to apply the credential to (e.g. `github.com`, `gitlab.example.com`) |
| `spec.type` | string | Yes | `token` for HTTP PAT/OAuth token, `sshKey` for SSH private key |
| `spec.secretRef` | string | Yes | Name of a StoredSecret holding the actual credential value |
| `spec.knownHosts` | string | No | `sshKey` only, at most 64 KiB. One or more OpenSSH `known_hosts` lines (`<host> <key-type> <base64-key>`) that pin the server's SSH host key. See [SSH host key verification](#ssh-host-key-verification). |

### SSH host key verification

For `sshKey` credentials, the controller always verifies the SSH server's host key when it fetches over SSH (AppSource syncs and `git://` templates); host key checking is never turned off there. Only a pinned `knownHosts` gives full protection: trust on first use cannot tell a genuine first connection from an attacker's, and `accept-new` also accepts an additional key type for a host it already knows.

- **`knownHosts` set (recommended):** ssh runs with `StrictHostKeyChecking=yes` against exactly the keys you pinned. Any other key fails the fetch. Get the lines with `ssh-keyscan -t ed25519 gitlab.example.com` and compare the fingerprint with the one your Git host publishes before pinning. For a non-default port use the `[host]:port` form.
- **`knownHosts` empty:** trust on first use (`StrictHostKeyChecking=accept-new`). The first key a host presents is recorded in the controller's persistent known_hosts file (`--git-known-hosts-file` / `UNIFIED_GIT_KNOWN_HOSTS_FILE`, default `<data-dir>/.ssh/known_hosts`, else a private per-process temporary directory that is lost on restart). The directory must be owned by the controller's user and not group- or other-writable (Unix), and the path must not contain a double quote; the controller refuses to start otherwise) and a key that later changes is rejected. If the file is not on persistent storage, or you run several controller replicas, each starts with an empty trust store, so the first connection after a restart is trusted again; pin `knownHosts` to avoid that.

A rejected key fails with an error containing `SSH host key verification failed`. If the key changed legitimately, update `knownHosts` (or delete the stale line from the controller's known_hosts file).

**Not covered:** the shipped `templates/git-checkout.yaml` and `templates/rsync-deploy.yaml` run on agents, not in the controller, and they do **not** verify host keys (they pass `StrictHostKeyChecking=no` and discard known_hosts). This field does not affect them; see TODO.md for the follow-up.

### Credential matching

When resolving a `git://` URI or AppSource `repoURL`, the controller looks up a GitCredential whose `spec.host` matches the URI's hostname. This allows job definitions to reference private templates without embedding credentials. An AppSource `repoURL` that embeds credentials (`https://user:pass@host/...`) is rejected; a GitCredential is the only way to authenticate an AppSource.

### Examples

```yaml
---
# GitHub PAT
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: github-org
spec:
  host: github.com
  type: token
  secretRef: GITHUB_TOKEN        # created with: unified-cli secret set GITHUB_TOKEN ghp_...

---
# GitLab SSH key
apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gitlab-internal
spec:
  host: gitlab.example.com
  type: sshKey
  secretRef: GITLAB_SSH_KEY      # created with: unified-cli secret set GITLAB_SSH_KEY -f ~/.ssh/id_ed25519
  knownHosts: |                  # optional: pin the host key (ssh-keyscan -t ed25519 gitlab.example.com)
    gitlab.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI...
```

Then reference in a job:

```yaml
steps:
  - name: build
    uses:
      job: git://github.com/my-private-org/ci-templates/jobs/build.yaml@v1.0.0
      with:
        target: ./cmd/server
# Credentials for github.com are resolved automatically via the GitCredential above
```

---

