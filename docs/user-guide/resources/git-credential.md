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
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique credential name |
| `metadata.annotations` | map[string]string | No | Arbitrary key/value metadata |
| `spec.host` | string | Yes | Hostname to apply the credential to (e.g. `github.com`, `gitlab.example.com`) |
| `spec.type` | string | Yes | `token` for HTTP PAT/OAuth token, `sshKey` for SSH private key |
| `spec.secretRef` | string | Yes | Name of a StoredSecret holding the actual credential value |

### Credential matching

When resolving a `git://` URI or AppSource `repoURL`, the controller looks up a GitCredential whose `spec.host` matches the URI's hostname. This allows job definitions to reference private templates without embedding credentials.

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

