# Migrating: host agents start without a native shell

Host (VM) agents on Windows used to exit at startup with `shell check failed ... git bash not found`
when Git for Windows was missing, even though only `native: true` jobs use bash (isolated jobs run
in Linux containers through the `ucd-sh` shim). On Linux and macOS a missing `bash` was only found
when a native step ran. Every OS now behaves the same way:

| Host state | Before | Now |
|---|---|---|
| Windows, no Git Bash | The agent exited with `shell check failed`. | The agent starts, logs `no native shell: ...`, and does not advertise `native`. |
| Linux/macOS, no `bash` on `PATH` | The agent advertised `native`; native steps failed at run time. | Same as Windows: warning, no `native` capability. |
| No native shell and no container runtime | Windows exited; Linux/macOS started with nothing usable. | The agent refuses to start: `no native shell (bash) and no container runtime are available: ...`. |

## What you must do

Nothing for agents that have a shell. If you relied on the Windows agent failing fast to catch a
missing Git Bash, watch for the `no native shell` warning in the agent log or check that `native` is
in the agent's capabilities instead. A `native: true` job whose only candidate agents lack a shell
stays `Queued` (see the [unschedulable
warning](../../troubleshooting/runs-and-scheduling.md#job-stays-queued-unschedulable-warning)).
Install Git for Windows (or bash) and restart the agent to enable native jobs.

See [Native shell not found](../agents.md#native-shell-not-found).
