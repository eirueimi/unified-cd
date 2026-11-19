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

Do not run old and new agent replicas side by side. During a mixed rollout the old replicas keep
writing to the legacy claim while the new ones use the new, empty claim, so anything written to the
legacy claim after you copy the data is lost. Scaling the Deployment to zero before the upgrade
(step 3) avoids this. The pod label changed with the name too, so a new agent does not recognise a
run pod of an old agent as holding a volume.

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
3. **Stop the agent**: let the in-flight runs finish, then scale the k8s-agent Deployment to zero
   so nothing holds either claim and no new run can start. Do not scale the upgraded agent up just
   to create the new claim: it could hand a queued run the empty volume before the copy. Create the
   new claim yourself instead, with the same size, access mode and storage class as the old one and
   these labels and annotation (the agent uses an existing claim as it is, and reclaims it later
   only if it carries the owner label):

   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: ci-main.go-cache
     namespace: <namespace>
     labels:
       unified-cd/volume-owner: ci-main      # the agentName
       unified-cd/volume: go-cache           # the volume name
     annotations:
       unified-cd/reclaim-policy: Retain     # or Delete, as in the AgentVolume
   spec:
     accessModes: [ReadWriteOnce]            # ReadWriteMany for a shared volume
     resources:
       requests:
         storage: 200Gi
   ```
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
   data, and delete the pod. Run the copy as root (the busybox default, as in the manifest) so `cp -a`
   preserves file ownership. If Pod Security forbids root, set `securityContext.runAsUser` to the
   UID your jobs use and `fsGroup` to match; ownership is then not preserved, only readable by that
   user. Scale the upgraded agent up only after the copy is done.
5. **Delete the old claims** once the new volume is verified:

   ```bash
   kubectl -n <namespace> delete pvc <agentName>-<volume>
   ```

   The agent will not do this for you. Deleting a claim whose volume is still in use by a pod does
   not complete until the pod is gone.
6. **Update anything that names the claim.** A `jobPodPolicy.allowPersistentVolumeClaims` entry,
   monitoring, or a backup job that referred to `<agentName>-<volume>` must now use
   `<agentName>.<volume>`. Only the current `<agentName>.<volume>` claims are reserved for named volumes
   (a job that mounts one directly is refused with `k8s: job pod policy: ...`). From 0.17 a
   legacy `<agentName>-<volume>` claim is an ordinary PVC: a job can mount it only if it is listed
   in `allowPersistentVolumeClaims`.

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
