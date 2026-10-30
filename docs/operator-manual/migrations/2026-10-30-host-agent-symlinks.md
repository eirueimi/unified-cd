# Migrating: the host agent archives symlinks as links, like the Kubernetes agent

The host agent used to archive a symlink by reading the file it points at, which failed with
`archive/tar: write too long` (a link to a non-empty file), `is a directory` (a link to a directory)
or `no such file or directory` (a dangling link). A cache entry or artifact with links in it could
therefore never be saved by a host agent (a `cache:` of `node_modules` or `.venv` was silently not
saved), and a host agent restoring an archive written by a Kubernetes agent turned each link into an
empty regular file. The host
agent now handles links exactly as the Kubernetes sidecar does. Nothing that worked before stops
working, except the narrow case in the last row.

| A job that... | Before (host agent) | Now (both agents) |
|---|---|---|
| uploads an artifact, or saves a cache, of a tree that contains a symlink | The step failed (artifact) or the cache was not saved. | The link is stored as a link entry; its target is not read. A link to a directory and a dangling link are fine. |
| restores a cache whose archive holds symlink or hardlink entries | Each link entry was written as an empty regular file (the link target was discarded) and the restore still reported a hit, with no warning. | The link entries are skipped, the restore is still a hit, and one `cache restore warning: skipped N symlink/hardlink entries; ...` line goes to the run log. Re-create what you need (`.venv`, `node_modules/.bin`) after the restore. |
| downloads an artifact whose archive holds a link entry | Failed with `unsupported symlink entry`. | Unchanged. |
| writes an artifact or cache into a destination that contains a symlink pointing outside the destination | The write followed the link. | The step fails (artifact), or the cache restore fails partway and is reported as *not restored* (files extracted before the offending entry stay). An extraction never writes through a symlink that leads out of its destination. |

The cache warning text changed from "not restored on the Kubernetes agent" to "not restored by the
agents"; match on the `cache restore warning: skipped` prefix if you grep for it.

## Cache entries restored by older host agents

A cache restored by an older host agent may contain empty regular files where the archive held
links (for example `node_modules/.bin/*`). Those stubs shadow the real tools until the cache key
changes. If that matters, bump the cache `key` or delete the entries from the store.

## Remaining differences

- A top-level `path` that is itself a symlink to a directory is archived as the target's contents on
  the host agent. The Kubernetes sidecar archives a lone link entry instead. Archive the real
  directory rather than a link to it.
- A single-file upload needs read permission on the file's parent directory on the host agent
  (the parent is opened as the confined root); search permission alone is no longer enough.
- Windows directory junctions are not supported in an archived tree.
