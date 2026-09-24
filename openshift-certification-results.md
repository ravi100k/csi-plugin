# OpenShift certification results — 2026-09-24

Status: **both profiles are green. NFS 40 pass / 0 fail, block 64 pass / 0 fail.**
The two file-subPath failures that survived every earlier fix are gone, as is
the capacity failure. The fix for subPath replaces the node's shared
root-export mount with one direct NFS mount per volume. Nothing is committed;
the per-change reasons are in `openshift-certification-code-changes.md`. No
Partner Connect activity.

## Environment

| | |
| --- | --- |
| Cluster | OpenShift 4.22.13 / Kubernetes 1.35.6, single node `00-50-56-ad-47-02` |
| Backend (NFS run) | Hammerspace Anvil `10.200.109.40`, data portal `10.200.109.43` (NFS_V4_1 and NFS_V3 up) |
| Backend (block run) | Hammerspace Anvil `10.200.105.164`, three data portals (`10.200.107.170`, `10.200.108.29`, `10.200.107.161`), each NFS_V4_1 and NFS_V3. `.40` became unreachable after the NFS run. |
| Driver image | NFS: `stagefix-20260924`. Block: `cleanup-20260924` (adds the dead-code removal) |
| Operator image | `localhost/hammerspace-csi-operator:cap-on-20260922` |
| Branch | `operator-code` @ `6e5bd24` + uncommitted working tree |

## Results

| Profile | Passed | Failed | Skipped | Duration | Driver image |
| --- | --- | --- | --- | --- | --- |
| NFS | **40** | **0** | 252 | 81m | `stagefix-20260924` |
| Block | **64** | **0** | 226 | ~20m | `cleanup-20260924` |

Against 2026-09-23 (NFS 37/3):
- **subPath:** all 16 tests pass, including `file as subpath` and `readOnly file
  specified in the volumeMount`, which failed in every earlier full run.
- **Capacity:** `capacity provides storage capacity information` passes.
- **LUN Overflow:** passed in 53m59s against its 60m limit (see below).

The suite exited non-zero only because of **cluster monitor checks, not driver
tests**:

| Monitor failures | Cause | Driver? |
| --- | --- | --- |
| 51 × `kubelet-container-restarts` | One per OpenShift system namespace; the monitor window reaches back to 2026-09-17. `hammerspace-csi` isn't among them and its pods have 0 restarts. | No |
| 24 × platform checks (`required-scc-annotation`, `termination-message-policy`, `lease-checker`, etcd/node/alerts invariants) | OpenShift's own workloads | No |
| `image-registry-availability` | The internal registry is removed on this lab cluster | Cluster setup |
| 2 in `kube-system` | Pod `hscsi-nfsdiag9`, a leftover diagnostic pod from 09-22. Now deleted. | Leftover |
| Pathological events in the LUN Overflow namespace | `ExternalProvisioning ... Waiting for a volume` repeated up to 84× per PVC while the loaded Anvil created shares at ~4 per minute | Backend throughput |

### Block run notes

- **Same result as before:** block matches its 2026-09-23 result (64/0), now
  with the staged design and the root-export code removed. The pre-flight gate
  and the suite show that file-backed and block volumes work with staging
  reduced to a no-op.
- **Monitor failures:** 77, the same cluster-wide set as in the NFS run. The
  only one naming the driver is the "pathological events" check on
  `statefulset/csi-provisioner`. Its delete/create events have been counted since
  2026-09-22 and grow by one per driver image rollout; the latest was at 12:21.
  The driver had 0 container restarts during the run.
- **Image changed mid-run:** at about 12:21, the operator's
  `RELATED_IMAGE_DRIVER` was changed to `clean-20260924`, outside this harness.
  Only the last test, LUN Overflow, was still running then, and it passed.
  Every other test finished on `cleanup-20260924`.
- **Before the run:** four NFS mounts to the unreachable `.40` Anvil were left on
  the node: the legacy root export and three backing shares. They were lazily
  detached, so the same-named backing share on the new Anvil could not hit a
  dead mount.

## Moving off the root-export bind design

### The old design

`NodeStageVolume` mounted the Hammerspace root export (`/`) once per node at
`/var/lib/hammerspace/rootmount`. Each share under it is an NFS junction, which
the kernel NFS client turns into an automounted submount the first time it is
traversed. `NodePublishVolume` bind-mounted `rootmount/<share>/` into the pod.
So a pod's volume was a bind of an automounted submount inside a node-wide
mount.

### Why file subPaths failed

For a file subPath, kubelet opens the file and then runs
`mount --bind /proc/<pid>/fd/N <target>`. Under the old design that bind failed
with `ESTALE` (`failed to prepare subPath for volumeMount`). Directory subPaths,
which kubelet prepares differently, passed.

What was established:
- `ESTALE` appeared only when the volume was reached through a root-export
  junction.
- It disappeared when the share was mounted directly.

The exact kernel mechanism was not isolated. The main suspect: junction
submounts are shrinkable automounts
(`/proc/sys/fs/nfs/nfs_mountpoint_timeout=500`), so the dentry the file
descriptor points into can be invalidated underneath kubelet.

### What was tried first

Only the full-run results are conclusive. Several changes passed targeted runs
and then failed in a full run.

| Attempt | Evidence | Result |
| --- | --- | --- |
| Root-mount lifecycle lock (serialize stage/unstage/publish on the shared root) | 09-23 full runs | Both file-subPath tests still failed. The lock was independently justified, but it wasn't the cause. |
| Stop remounting the shared root per volume (your change) | `userfix1.log` | Both failed |
| Recursive bind (`rbind`) from the root export | `rbind1.log` | Both failed |
| Make periodic republish (`requiresRepublish: true`) a no-op when already published, so it stops remounting the root under live pods | `republish1.log` | Both failed |
| `lookupcache=none` / `lookupcache=pos` on the root mount | `lc1`, `lc2`, `lcpos1` targeted | Passed targeted runs |
| … the same, in a full run | `final-nfs.log` (37/3) | Both file-subPath tests failed again, so it was removed |
| **Staged design: direct per-volume mount** | `stage1.log`, then targeted `subpathstage-20260924` | **Passed, with 0 `ESTALE`** |
| … the same, in a full run | `stagefix-full-nfs.log` | **40/0, all 16 subPath tests pass** |

### The new design

| Volume type | Stage | Publish |
| --- | --- | --- |
| Native NFS | Mount the share directly at the CSI staging path (`.../globalmount`) | Bind the staging path into the pod; kubelet makes subPath binds beneath it |
| File-backed ext4/xfs, raw block, NFS inside a backing share | Nothing | Mount the backing share on demand, then attach the backing file through it, as before |

The node no longer mounts the root export at all. For volumes staged by an
older driver version, `NodeUnstageVolume` still cleans up their markers and the
root mount.

### Verified on the node with `deploy-test/paypal/pod3.yaml`

This is a pre-provisioned PV for `/share2`, one pod, and 100 subPath mounts
(`cosmos1/user0..99`). Read from `/proc/1/mountinfo` in the host namespace:

| Role | Count | Kind |
| --- | --- | --- |
| Staging `globalmount`, `10.200.109.43:/share2`, type `nfs` | 1 | **Actual NFS mount** |
| CSI publish into the pod | 1 | Bind of the staging mount |
| kubelet subPath mounts | 100 | Binds |
| **Total** | **102** | **1 NFS mount + 101 binds**, all on one superblock (`0:426`) |

- **Write path:** a file written in the pod was read back on the node through the
  staging NFS mount, and all 100 `userN` directories were present there.
- **Teardown:** deleting the pod and PV left 0 CSI NFS mounts on the node.

### A data-loss bug found and fixed during the switch

The first staged build ran pod3 with **0 NFS mounts**. Every entry was on
`8:4` (`/dev/sda4`, the node's local disk), and `share2` stayed empty.
- **Cause:** the static PV has no `fsType`. Stage didn't recognise it as native
  NFS and mounted nothing; publish defaulted it to `nfs` and bound the empty
  staging directory.
- **Effect:** the pod ran and wrote successfully, all to local disk.
- **Fix:** stage and publish now share one fsType resolution, and publish refuses
  to bind a staging path with nothing mounted on it. The table above is the
  re-run.

### What the new design costs

- **More mounts:** one NFS mount per volume per node instead of one per node.
  260 volumes on one node (LUN Overflow) worked.
- **Mounts wait for Anvil publishing:**
  - **Stage needs a published share.** A share can be staged only after the
    Anvil has published it (`shareState: PUBLISHED`, visible in
    `showmount -e`), not just created it (`MOUNTED`, which is when
    `share-create` completes and the PV binds).
  - **Behaviour on a loaded Anvil:** that gap exceeded 8 minutes during LUN
    Overflow. NodeStage failed with `could not mount to any data-portals` and
    kubelet retried until the share was published.
  - **LUN Overflow timing:** the test passed in 53m59s. The two passing runs on
    09-23 took 34 and 36 minutes, but under different Anvil load, so this isn't
    a like-for-like comparison.
  - **Decision (2026-09-24):** no driver-side wait or root-export fallback.
    Kubelet's retries handle it.
- **Upgrade:** a new pod on a node where the old driver already staged the same
  volume is refused until that volume is unstaged there. This is untested. See
  "Known issues" in the code-changes document.

## Anvil throughput observed

- **Share creation:** about 4 per minute on `.40` during LUN Overflow.
- **Share deletion after the run:** the 260 LUN Overflow shares hit
  `400 Task 'share-delete' is already running ... VALIDATED/EXECUTING`. The
  provisioner retries a delete while the first one is still queued on the Anvil.
  The drain had removed 1 of 260 after about 20 minutes, and `/tasks` did not
  answer within 60s.
- **Lesson:** leave the Anvil time to drain between profiles.

## Artifacts

- `/tmp/hscsi-cert-20260923/nfs/stagefix-full-nfs.log`: the green NFS run
- `/tmp/hscsi-cert-20260923/nfs/results/`: its JUnit XML and HTML summaries
- `/tmp/hscsi-cert-20260923/nfs/{userfix1,rbind1,republish1,lc1,lc2,lcpos1,stage1}.log`: the targeted attempts in the table above
- `/tmp/hscsi-cert-20260923/nfs/results-prev-070511/`: the previous NFS results

## Not done

- Upgrade from the root-export design not tested
- Multi-node behaviour unexercised (single-node cluster)
- KubeVirt storage checkup not started
- Nothing committed; no Partner Connect submission

---

# OpenShift certification results — 2026-09-23

Status: **block profile is green — 64 pass / 0 fail.** NFS is at 37 pass / 3 fail,
down from 7 failures, with two distinct causes remaining (subPath file-subpath,
and capacity publication latency). Five driver defects were found and fixed
today; two of them were latent production bugs rather than test artifacts. No
submission, no Partner Connect activity. **Nothing is committed** — every driver
change below is in the working tree on `operator-code` awaiting review.

## Environment

| | |
| --- | --- |
| Cluster | OpenShift 4.22.13 / Kubernetes 1.35.6, single node `00-50-56-ad-47-02` |
| Backend | Hammerspace Anvil `10.200.109.40`, DSX data portals on `10.200.109.43` |
| Driver image | `localhost/hammerspaceinc/csi-plugin:certfix5-20260923` (UBI 9 base) |
| Operator image | `localhost/hammerspace-csi-operator:cap-on-20260922` |
| Branch | `operator-code` @ `7f42191` + uncommitted working tree + `d1df222` applied for testing |
| NFS versions | Anvil serves 4.2; data portal serves 4.1 and v3 only. Backing-share mounts land on v3 |
| Capability manifests | `capacity: true` both profiles; `snapshotDataSource: false` on NFS |

Both profiles were run from a fully clean base: Anvil reduced to `root` and
`share1`, cluster at 0 PVs / 0 PVCs / 0 e2e namespaces / 0 snapshot contents,
and the pre-flight gate green on all four StorageClasses.

## Results

| Profile | Passed | Failed | Skipped | Duration | Driver image |
| --- | --- | --- | --- | --- | --- |
| Block | **64** | **0** | 226 | ~16m | `certfix5-20260923` |
| NFS | 37 | 3 | 252 | ~65m | `certfix5-20260923` |

Against the 2026-09-22 baseline (NFS 41/7, block 53/11). Block's pass count rose
and its failures went to zero. NFS's pass count *fell* from 41 to 37 because
snapshot tests no longer generate for that profile at all — see the
`snapshotDataSource` change below. That is a capability-declaration correction,
not a regression.

## Driver defects found and fixed

| # | Defect | Symptom | Severity |
| --- | --- | --- | --- |
| 1 | `UpdateShareSize` wrote to a nil map | Panicked the whole controller process, killing every in-flight RPC, whenever a share was absent | Crash |
| 2 | `RestoreFileSnapToDestination` has no size parameter | File-backed/block volumes restored from a snapshot silently kept the snapshot's size, ignoring a larger requested PVC capacity | Data/capacity |
| 3 | `ListSnapshots` compared a composite handle against a bare backend name | Pre-provisioned `VolumeSnapshotContent` could never become ready; file-backed snapshots were invisible to the scan entirely | Functional |
| 4 | `NodeExpandVolume` passed the mount path to `resize2fs` | `resize2fs` requires a block device and exits 1 on a directory, so **every ext4 node expansion failed**. XFS masked it, because `xfs_growfs` wants the mount point | Functional |
| 5 | `DeleteFileSnapshot` read the wrong path component for the timestamp | File snapshots **silently survived their own deletion**, then permanently blocked `DeleteVolume` on the source volume with `VolumeDeleteHasSnapshots` | Storage leak |

Defects 4 and 5 are the significant ones, and neither is specific to the test
suite:

- **#4 lives in `d1df222` (PR #77)**, not in this session's work. It must land in
  that PR or a rebase will reintroduce it.
- **#5 leaked backend storage permanently.** Every file-backed snapshot ever
  taken remained on the Anvil, and its source volume could never be deleted. The
  fix stops new orphans; snapshots already stranded by the old code cannot be
  recovered by it, because the CO has long since deleted their `VolumeSnapshot`
  objects and will never call `DeleteSnapshot` again. Those must be cleared at
  the backend.

### Root cause of #5, in detail

Hammerspace lays file snapshots out as:

```
<share>/.fsnapshot/<source-file-name>/<timestamp>
```

`DeleteFileSnapshot` assumed the reverse (`<timestamp>/<file>`) and read the
second-to-last component, yielding the source *file name*. That was then
truncated into a nonsense "date" — `hscsi-cert-block-20260923-pvc` — and sent as
`date-time-expression`. The API matched nothing and answered 400, which the
caller treats as success. Confirmed by querying the Anvil directly and finding
the snapshot still present after a reported-successful delete.

`fileSnapshotTimestamp` now locates the timestamp by pattern rather than by
position, handles both layouts, and errors instead of silently no-op'ing.

## Harness corrections (not driver changes)

Three of the 2026-09-22 failures were the harness testing things it should not
have, or testing them in a configuration we do not ship:

| Change | Why |
| --- | --- |
| NFS `snapshotDataSource: true` → `false`, `SnapshotClass` removed | Snapshot and restore of share-backed NFS volumes is not a shipped feature. Declaring it made the suite generate restore-to-larger and pre-provisioned-snapshot tests against NFS. Removes 3 baseline failures. |
| Both profiles `volumeBindingMode: Immediate` → `WaitForFirstConsumer` | The external-provisioner's capacity controller **skips `Immediate` classes outright** (`ignoring storage class X because it uses immediate binding`), so no `CSIStorageCapacity` object is ever published and the capacity test cannot pass. The Operator already ships `WaitForFirstConsumer`; certifying `Immediate` tested a configuration we do not ship. |
| NFS `vers=4.2` → `vers=4.1` | The data portal serves `NFS_V4_1` only; 4.2 is rejected with `Protocol not supported`. |

The 2026-09-22 note attributing the capacity failure to a missing
`--capacity-poll-interval` was **wrong**. Publication is event-driven and takes
~0.12s for a new class; the poll interval was never the constraint. The binding
mode was.

## Driver behaviour confirmed, not changed

`NFS 4.2 → v3` fallback in `MountShareAtBestDataportal` looks like it is missing
a 4.1 step, and on this backend every data-portal mount does fall through to v3.
Adding a 4.1 step was tried and **reverted**: `qemu-img` takes an OFD byte-range
lock when creating a raw image, and this backend's NFSv4.1 export does not
support byte-range locking, so every file-backed `CreateVolume` fails with:

```
qemu-img: Failed to lock byte 101: Unknown error 524   (ENOTSUPP)
```

Verified directly against the same share over both versions — 4.1 fails, v3
succeeds. NFSv3 mounts with `nolock`, keeping locking local. The fall-through to
v3 is therefore load-bearing, and the reasoning is recorded in the code so it is
not "fixed" again. A 4.1 step only becomes safe once raw-file creation stops
depending on `qemu-img`'s locking (`truncate`/`fallocate` would size a sparse
file without any lock).

Note the split: the **Anvil** (`10.200.109.40`) does serve 4.2 — the root export
mounts at `vers=4.2`. Only the **data portal** (`10.200.109.43`) is 4.1-only.

## Remaining NFS failures

### 1. subPath file-as-subpath — 2 tests

```
failed: subPath should support file as subpath [LinuxOnly]
failed: subPath should support readOnly file specified in the volumeMount [LinuxOnly]
```

kubelet reports `failed to prepare subPath for volumeMount`. **14 of 16 subPath
variants pass**, including `readOnly file`, `existing directory`, `restarting`
and `unmount after directory deleted`.

The 2026-09-18 diagnosis — a concurrency race on root-mount teardown — was
**wrong**. A root-mount lifecycle lock was implemented against it
(`acquireRootMountLock`, serializing `NodeStageVolume` / `NodeUnstageVolume` /
`publishShareBackedVolume`) and subPath still fails. That lock is independently
justified (the shared root mount genuinely had no serialization) but should not
be credited with fixing subPath.

That exactly the two *file*-subpath variants fail, while every directory variant
passes, points to a structural cause rather than a timing one — consistent with
the earlier 3-layer mount chain theory: each volume is a nested NFS submount
inside the shared root mount, `publishShareBackedVolume` bind-mounts it a second
time, and kubelet's own `/proc/<pid>/fd` bind adds a third. Not yet proven.

### 2. Storage capacity information — 1 test

```
fail [capacity.go:135]: Timed out after 60.000s.
no CSIStorageCapacity objects for storage class "e2e-capacity-5046-e2e-scgq2vd"
```

Passes on block (1.4s), fails on NFS. Both classes are `WaitForFirstConsumer`,
so this is not the binding-mode issue. The likely cause is throughput: each
`GetCapacity` refresh costs a ~1.5s Anvil round-trip and the controller works
through classes serially, so with dozens of e2e StorageClasses alive during the
longer NFS run a newly created class waits behind the queue and misses the 60s
budget. The same latency explains `FailedScheduling: did not have enough free
storage` events seen on unrelated pods during the run.

If confirmed, the fix is to cache cluster capacity in `GetCapacity` (the driver
already has `pkg/common/hs_cache.go`) so a refresh does not cost a round-trip.

## Block profile progression

| Run | Result | Change |
| --- | --- | --- |
| 09-22 baseline | 53 pass / 11 fail | — |
| run 1 | 54 pass / 2 real | first fixes |
| run 2 | 0 pass / 22 fail | NFS 4.1 regression (reverted) |
| run 3 | 56 pass / 1 real | 4.1 reverted |
| run 4 | 60 pass / 4 fail | ext4 expansion fixed (#4) |
| **run 5** | **64 pass / 0 fail** | snapshot deletion fixed (#5) |

Confirmed working on block: restore-to-larger, snapshot data source (fs and
block), ephemeral snapshots (both policies), pre-provisioned snapshots (both
policies), dynamic snapshots (both policies), capacity, all six expansion tests,
and LUN overflow.

Deletion behaviour after #5: `refusing to delete volume ... snapshot(s) still
exist` still appears during a run, but is now **transient** — the CO issues
`DeleteVolume` and `DeleteSnapshot` concurrently, so the volume delete is
legitimately refused until the snapshot is gone and then succeeds on retry.
Before the fix the refusal never cleared. Independently confirmed during
cleanup: every `Delete`-policy PV drained within 15s unprompted.

## Environment notes

- **NFSv4.2 is not available on the data portal.** Only `NFS_V3` and `NFS_V4_1`
  portals are UP, and `portalFloatingIps` is empty.
- **Anvil share deletion needs `?delete-path=true&delete-delay=0`.** A bare
  `DELETE /shares/<name>` is accepted (202) but leaves the share to prune on the
  default ~24h delay.
- **`GET /shares/<name>` returning 200 does not mean the create task finished.**
  A following task then fails with `BUSY, task 'share-clone' conflicts with
  running task 'share-create(<uuid>)' which has a status of EXECUTING`. Wait on
  the task from the create's `Location` header.
- **Shares at nested paths are supported**, with their own `shareSizeLimit`, and
  the Anvil auto-creates the directory. Verified by direct probe.

## Artifacts

- `/tmp/hscsi-cert-20260923/block/run5.log` — the green block run
- `/tmp/hscsi-cert-20260923/nfs/run1.log` — the NFS run
- `/tmp/hscsi-cert-20260923/{nfs,block}/results/` — JUnit XML, HTML summaries, monitor JSON
- `/tmp/hscsi-cert-20260923/{nfs,block}/manifest.yaml` — capability manifests used
- `/tmp/hscsi-cert-20260923/{nfs,block}/storageclass.json` — StorageClasses used
- `/tmp/hscsi-cert-20260923/gate.sh`, `run-suite.sh`

Earlier block runs (`run.log` ... `run4.log`) are kept in the same directory for
the progression table above. Artifacts stay outside the repository by convention.

## Not done

- Nothing committed; all driver changes are uncommitted in the working tree
- `d1df222` (PR #77) applied to the working tree **for testing only** — its
  `resize2fs` bug (#4) still needs fixing in that PR
- subPath file-as-subpath not root-caused
- Capacity latency on NFS not confirmed or fixed
- Multi-node behaviour unexercised (single-node cluster)
- KubeVirt storage checkup not started
- No Partner Connect submission

---

# OpenShift certification results — 2026-09-22

Status: both profiles ran to completion against a healthy environment. NFS 41 pass /
7 blocking failures; block 53 pass / 11 blocking failures (5 of those 11 are the
suite's own abort, not real). No submission. Code committed and pushed to
`operator-code`; no Partner Connect activity.

Earlier dated sections for 2026-09-17 and 2026-09-18 live on the
`driver-fixes-wip-20260919` branch's copy of this file and should be reconciled
with this one when that branch merges.

## Environment

| | |
| --- | --- |
| Cluster | OpenShift 4.22.13 / Kubernetes 1.35.6, single node `00-50-56-ad-47-02` |
| Backend | Hammerspace Anvil `10.200.109.40`, DSX data portals on `10.200.109.43` |
| Driver image | `localhost/hammerspaceinc/csi-plugin:capfix2` (UBI 9 base) |
| Operator image | `localhost/hammerspace-csi-operator:cap-on-20260922` |
| Branch | `operator-code` @ `7f42191` |
| Capability manifests | `capacity: true` on both profiles (first run with capacity tracking enabled) |

## Results

| Profile | Passed | Blocking failures | Monitor failures | Skipped | Duration |
| --- | --- | --- | --- | --- | --- |
| NFS | 41 | 7 | 4 | 244 | 1h10m29s |
| Block | 53 | 11 | 2 | 226 | 29m09s |

Trend against 2026-09-18: NFS 15 → 7, block 16 → 11. The improvement came from
repairing the environment, not from driver changes — no driver fix landed between
those runs. Several cases that failed on 09-18 now pass, including
AllowedTopologies scheduling, generic ephemeral block volumes, multiVolume
retain-across-recreate and several subPath variants.

## Caveat: five block failures are the suite aborting, not defects

`openshift-tests` aborts once failures cross its mass-failure threshold
(block hit 13 against a threshold of 10) and marks whatever was in flight at
parallelism 10 as failed with `Interrupted by User`:

- Generic Ephemeral-volume expansion of pvcs created for ephemeral pvcs
- volume-expand should resize volume when PVC is edited while pod is using it
- Dynamic Snapshot (delete policy) snapshottable
- Dynamic Snapshot (retain policy) snapshottable
- multiVolume concurrently access the volume and restored snapshot on the same node

Block's real blocking-failure count is therefore about 6. These five need a rerun
before any verdict; they are neither confirmed passes nor confirmed failures.

## Failures with recorded errors

### 1. Snapshot restore to a larger PVC — NFS and block

```
Expected <int>: 1073741824 to be > <int>: 1073741824   (NFS)
Expected <int>: 1020702720 to be > <int>: 1020702720   (block)
```

The restored volume is exactly the source size. Confirms the already root-caused
bug: `CreateShareFromSnapshot` (pkg/client/hsclient.go) takes a `size` parameter
and never applies it. Fix is to call the existing `UpdateShareSize` after a
successful clone when the requested size exceeds the source.

### 2. Pre-provisioned snapshot never becomes ready — NFS x2, block x2

```
fail [.../snapshot_resource.go:157]: VolumeSnapshot
pre-provisioned-snapshot-<uuid> is not ready within 5m0s
```

Imported snapshots do not reach `readyToUse` inside five minutes. Affects both
delete and retain deletion policies.

### 3. subPath — NFS x2

```
failed to prepare subPath for volumeMount "test-volume" of container
"test-container-subpath-dynamicpv-..."  reason: CreateContainerConfigError
```

The previously root-caused ESTALE case: each volume is already a nested NFS
submount inside the shared root mount, `publishShareBackedVolume` bind-mounts it a
second time, and kubelet's own fd bind-mount adds a third layer. Fully
reproducible, not a race.

### 4. Mount options (`noatime`) — NFS

```
fail [.../provisioning.go:1081]: Told to stop trying after 8.059s.
pod "pvc-volume-tester-reader-..." failed with status
```

Matches the root-caused race in `EnsureRootExportMounted`: the node-wide shared
root path is only mounted if not already mounted, so a volume's own mountFlags are
silently ignored when another volume established the mount first.

### 5. Storage capacity information — NFS and block (new)

```
fail [.../capacity.go:135]: Timed out after 60.001s. after creating storage class
no CSIStorageCapacity objects for storage class "e2e-capacity-<id>"
```

New this run, introduced by declaring `capacity: true` after enabling capacity
tracking. The test creates a *new* StorageClass and allows 60s for
`CSIStorageCapacity` objects to appear for it; none do. This matches manual
observation, where objects took one to three minutes to publish. The provisioner
runs without `--capacity-poll-interval`, so it is on the default cadence, which
exceeds the test's budget.

This is **not** the topology limitation initially suspected — `GetCapacity` does
ignore `AccessibleTopology`, but that is not what this test asserts.

### 6. Offline expansion and resize-then-recreate — block x2

Pod-status failures in the expansion path, consistent with commit `d1df222`
(XFS grow without pod restart, including the `NodeExpandVolume` fix that discovers
filesystem type from the mount point when the CSI `volume_capability` is absent)
having been reset off this branch.

## Consolidated view

Excluding the five interrupted tests, four distinct problems remain:

| Problem | Profiles | Fix status |
| --- | --- | --- |
| Snapshot restore (larger-size and pre-provisioned readiness) | NFS, block | Root-caused; restore-to-larger fix written on `driver-fixes-wip-20260919` |
| subPath ESTALE | NFS | Root-caused; fix on `driver-fixes-wip-20260919` |
| Mount options ignored | NFS | Root-caused; fix on `driver-fixes-wip-20260919` |
| Expansion | block | Fix is commit `d1df222`, currently off this branch |
| Capacity publication latency | NFS, block | New; likely `--capacity-poll-interval` tuning, not a code defect |

`driver-fixes-wip-20260919` is not merged into `operator-code` and carries roughly
1,300 lines across `pkg/driver/node.go`, `node_helper.go`, `utils.go` and tests.
Merging it plus `d1df222` is the single highest-leverage action against this list.

## Environment problems resolved during this session

Two environment faults produced two earlier unusable runs (NFS 2 pass / 41 fail,
block 0 pass / 62 fail) and are recorded here so they are not re-diagnosed:

1. **Node NFSv4 client wedged.** Stale `nfs4` mounts to retired backends poisoned
   `nfs4_discover_server_trunking` for *every* server, making four different Anvils
   look broken — including one that had worked an hour earlier. Symptom:
   `NFS: nfs4_discover_server_trunking unhandled error -1. Exiting with error EIO`,
   with mounts present but `/proc/fs/nfsfs/servers` empty. Clearing all NFS mounts
   recovered it once; after it recurred, only a node reboot cleared it (module
   refcounts stayed pinned with nothing mounted, so `modprobe -r` could not work).
   Proof it was client-side: the same `vers=4.2` mount to the same server succeeded
   from a different host at the same moment.
2. **No mountable address for the driver.** `portalFloatingIps` empty and the
   `NFS_V4_1` data portal `DOWN`, so the driver's floating-IP-then-data-portal
   fallback had nowhere to mount while provisioning still succeeded. File-backed and
   block volumes still worked (they mount the backing share directly); only
   share-backed NFS failed. Enabling the NFSv4 portal fixed it.

A failing suite run demonstrably re-wedges the node's NFS client, so failures
cascade into subsequent runs. Clear all NFS mounts between runs.

## Pre-flight gate

`/tmp/hscsi-cert-20260922/gate.sh` checks, in the order these actually break:
the driver's reach to the Anvil management API; whether any address the driver
would select actually serves NFS (floating IPs, else UP data portals); and
provision + publish + real I/O for all four StorageClasses. It reports in about two
minutes what the suite takes an hour to reveal, and exits non-zero on first
failure. Both unusable runs above would have been caught by its second check.

Verified green before these runs:

```
== 2. mountable data path
   portal UP: NFS_V3    10.200.109.43
   portal UP: NFS_V4_1  10.200.109.43
== 3. provision + publish + I/O per mode
   ok hammerspace-nfs    (Filesystem)
   ok hammerspace-ext4   (Filesystem)
   ok hammerspace-xfs    (Filesystem)
   ok hammerspace-block  (Block)
GATE PASS
```

## Artifacts

- `/tmp/hscsi-cert-20260922/nfs/results/` — JUnit `junit_e2e__20260922-162814.xml`, HTML summaries, monitor JSON
- `/tmp/hscsi-cert-20260922/block/results/` — JUnit `junit_e2e__20260922-152420.xml`, HTML summaries, monitor JSON
- `/tmp/hscsi-cert-20260922/{nfs,block}/manifest.yaml` — capability manifests used (both `capacity: true`)
- `/tmp/hscsi-cert-20260922/gate.sh`, `run-suite.sh`

Artifacts are outside the repository by convention.

## Operator bundle validation

The OLM bundle generated by `operator/hack/build_bundle.py` passes every
`operator-sdk bundle validate` suite, with zero errors:

`operatorframework` (default), `operatorhubv2`, `capabilities`, `categories`,
`good-practices`, `alpha-deprecated-apis`, `multiarch`, `community`.

Two non-blocking warnings remain: `csv.Spec.minKubeVersion` is not set, and
`csv.Spec.Icon` is not specified. The hand-written operator therefore produces a
conformant bundle; adopting `operator-sdk` scaffolding is a maintainability choice,
not a certification requirement.

## Not done

- `driver-fixes-wip-20260919` and `d1df222` not merged
- The five interrupted block tests not rerun
- KubeVirt storage checkup not started
- Multi-node behaviour unexercised (single-node cluster)
- No Partner Connect submission; Build-module access still outstanding
