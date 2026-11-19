# Migrating: Kubernetes volume PVC names

A Kubernetes agent used to name the PVC behind an `AgentVolume` `<agentName>-<volume>`. That name
is not unique: agent `a-b` with volume `c` and agent `a` with volume `b-c` both produced `a-b-c`,
so two agents in one namespace could end up sharing (or fighting over) one claim. The PVC is now
named `<agentName>.<volume>`. Agent names and volume names are DNS labels and never contain `.`, so
no two agent/volume pairs can produce the same claim.

This is a **breaking change for Kubernetes agents that already provisioned volumes**. Host agents
are not affected.

| | Before | Now |
|---|---|---|
| PVC name | `<agentName>-<volume>`, e.g. `ci-main-go-cache` | `<agentName>.<volume>`, e.g. `ci-main.go-cache` |
| Longest claim name | 63 characters; a longer one was reported not ready with `invalid: claim name "<claim>" exceeds 63 characters` | 127 characters (63 + `.` + 63); that error no longer exists |
| Pod label that marks a volume in use | `volume.unified-cd/<agentName>-<volume>: <volume>` | `volume.unified-cd/<volume>: <agentName>` |
| `agentName` that is not a DNS label | Refused at startup | Refused at startup; a delivered volume is also reported not ready with `invalid: agentName "<name>" is not usable in a PVC name: ...` if it ever gets that far |
| A PVC left from the old naming | Reused as it was | **Ignored.** Never adopted, patched, resized or deleted, whatever its `reclaimPolicy` annotation says |

After the upgrade, the agent creates the new claim **empty**. The old claim keeps its data, is never
reclaimed, and keeps costing storage until you delete it.

The pod label changed with the name, so a run pod created by an older agent is not recognised as
holding a volume by a new one. Let the runs that are in flight finish before you roll the agent,
or accept that a new run may start on a volume an old pod still holds.

## What you must do

1. **Decide per volume whether its data matters.** A cache that rebuilds itself needs nothing:
   start the new agent and delete the old claim (step 5). For a volume whose contents you want to
   keep, copy the data before the new agent runs a job on it (steps 2 to 4).
2. **Find the old claims.** They carry the owner label of the agent that created them:

   ```bash
   kubectl -n <namespace> get pvc -l unified-cd/volume-owner=<agentName>
   ```

   Old claims are the ones named `<agentName>-<volume>`. After the upgrade the new ones appear next
   to them as `<agentName>.<volume>`.
3. **Stop the runs that use the volume**, for example by scaling the k8s-agent Deployment to zero
   after the in-flight runs have finished, so nothing holds either claim. Then let the upgraded
   agent create the new claim (scale it back up and wait until the volume is ready, or create the
   PVC yourself with the same size, access mode and storage class, then scale the agent down
   again).
4. **Copy the data** with a one-off pod that mounts both claims. A `ReadWriteOnce` claim can only be
   mounted by pods on one node at a time, so if the two claims live on different nodes the pod may
   stay `Pending`; in that case copy through a pod pinned to the node that holds the old claim, or
   restore from a snapshot of the old volume:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     name: ucd-volume-copy
     namespace: <namespace>
   spec:
     restartPolicy: Never
     containers:
       - name: copy
         image: busybox:1.36
         command: ["sh", "-c", "cp -a /old/. /new/ && echo copied"]
         volumeMounts:
           - { name: old, mountPath: /old, readOnly: true }
           - { name: new, mountPath: /new }
     volumes:
       - name: old
         persistentVolumeClaim: { claimName: ci-main-go-cache }
       - name: new
         persistentVolumeClaim: { claimName: ci-main.go-cache }
   ```

   Apply it, wait for `kubectl -n <namespace> logs ucd-volume-copy` to print `copied`, check the
   data, and delete the pod. Run the copy as the user that owns the files (add a `securityContext`
   if your jobs run as a non-root user and the files must keep their owner; `cp -a` preserves
   ownership only when run as root).
5. **Delete the old claims** once the new volume is verified:

   ```bash
   kubectl -n <namespace> delete pvc <agentName>-<volume>
   ```

   The agent will not do this for you. Deleting a claim whose volume is still in use by a pod does
   not complete until the pod is gone.
6. **Update anything that names the claim.** A `jobPodPolicy.allowPersistentVolumeClaims` entry,
   monitoring, or a backup job that referred to `<agentName>-<volume>` must now use
   `<agentName>.<volume>`. A job pod that mounts a named-volume claim directly is still refused
   (`k8s: job pod policy: ...`), whichever spelling it uses.

## How to tell the migration is needed

After upgrading, a job that mounts a volume that used to hold data finds it empty, and
`kubectl -n <namespace> get pvc` shows both `ci-main-go-cache` (old, untouched) and
`ci-main.go-cache` (new, empty). Do not change `agentName` to get the old name back: that
provisions yet another set of claims.

## Related changes in the same release

- The agent's volume lock no longer waits for PVC API calls. A slow storage provisioner used to
  delay the creation of unrelated run pods on the same agent for as long as the PVC call took.
- The log line `volume left the set; reclaim scheduled after 10m0s` is written once per absence of
  a volume instead of at each stamp.

See [Named volumes on Kubernetes](../kubernetes-integration.md#named-volumes) and
[Volumes declared on the controller](2026-11-18-controller-declared-volumes.md).
