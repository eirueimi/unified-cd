# Migrating: the host agent archives symlinks as links, like the Kubernetes agent

The host agent used to archive a symlink by reading the file it points at, which failed with
`archive/tar: write too long` (a link to a non-empty file), `is a directory` (a link to a directory)
or `no such file or directory` (a dangling link). A cache entry or artifact with links in it could
therefore never be saved by a host agent (a `cache:` of `node_modules` or `.venv` was silently not
saved) and an archive written by a Kubernetes agent could not be restored by a host agent. The host
agent now handles links exactly as the Kubernetes sidecar does. Nothing that worked before stops
working, except the narrow case in the last row.

| A job that... | Before (host agent) | Now (both agents) |
|---|---|---|
| uploads an artifact, or saves a cache, of a tree that contains a symlink | The step failed (artifact) or the cache was not saved. | The link is stored as a link entry; its target is not read. A link to a directory and a dangling link are fine. |
| restores a cache whose archive holds symlink or hardlink entries | The restore failed partway (some files written) and was logged as *not restored*. | The link entries are skipped, the restore is still a hit, and one `cache restore warning: skipped N symlink/hardlink entries; ...` line goes to the run log. Re-create what you need (`.venv`, `node_modules/.bin`) after the restore. |
| downloads an artifact whose archive holds a link entry | Failed with `unsupported symlink entry`. | Unchanged. |
| writes an artifact or cache into a destination that contains a symlink pointing outside the destination | The write followed the link. | The step fails (artifact) or the entry is not restored (cache); an extraction never writes through a symlink that leads out of its destination. |

The cache warning text changed from "not restored on the Kubernetes agent" to "not restored by the
agents"; match on the `cache restore warning: skipped` prefix if you grep for it.
