# Driver code changes since `6e5bd24`, and why

This covers the uncommitted working tree on `operator-code`: every change, the
reason for it, and how it was verified. The test evidence and the investigation
behind these changes are in `openshift-certification-results.md` (2026-09-24
section).

Unit tests: `go test ./pkg/...` passes. `go vet` and `gofmt` are clean.

## 1. Native NFS volumes are staged with one direct mount per volume

**Files:** `pkg/driver/node.go` (`NodeStageVolume`, `NodeUnstageVolume`),
`pkg/driver/node_helper.go` (`publishShareBackedVolume`)

| RPC | Before | After |
| --- | --- | --- |
| `NodeStageVolume` | Wrote a marker, then mounted the Hammerspace root export (`/`) once per node at `/var/lib/hammerspace/rootmount` | Mounts the volume's own share at the CSI staging path (`.../globalmount`) with `MountShareAtBestDataportal` |
| `NodePublishVolume` | Bind-mounted `rootmount/<share>/` (an NFS junction submount) into the pod | Bind-mounts the staging path into the pod, and refuses if nothing is mounted there (see 2) |
| `NodeUnstageVolume` | Removed the marker, then unmounted the root export when no markers were left | Unmounts the staging path, and force-detaches it if it returns `EIO`/`ESTALE` |

**Why:** kubelet's file-subPath preparation failed with `ESTALE` whenever the
volume was reached through a root-export junction. Two certification tests
failed in every full run under the old design:
- `subPath should support file as subpath`
- `subPath should support readOnly file specified in the volumeMount`

Several narrower fixes were tried first and each failed; the results document
lists them. With a direct per-volume mount, all 16 subPath tests pass in a full
run and no `ESTALE` appears.

**What this costs:**
- **More NFS mounts:** one NFS mount per volume per node instead of one per node.
  LUN Overflow (260 volumes on one node) passed.
- **Mounts wait for Anvil publishing:** a share can be mounted only once the Anvil
  has published it (`shareState: PUBLISHED`), not just created it (`MOUNTED`). On
  a loaded Anvil that gap was over 8 minutes. You decided (2026-09-24) not to add
  a driver-side wait or fallback, because kubelet retries NodeStage until the
  share is published.

`publishShareBackedVolume` keeps a direct mount when there is no staging path.
This covers controller-side metadata mounts in `ensureShareBackedVolumeExists`
and `ensureBackingShareExists`, which now pass `""` as the staging path.

## 2. Stage and publish agree on fsType, and publish refuses an empty staging path

**Files:** `pkg/driver/node.go` (`nodeVolumeFsType`), `pkg/driver/node_helper.go`,
new `pkg/driver/node_fstype_test.go`

**Why:** this was a data-loss bug introduced with change 1 and caught before
commit.
- **The mismatch:** `NodeStageVolume` read fsType only from the capability. For a
  pre-provisioned PV with no `fsType`, it did not recognise the volume as native
  NFS and mounted nothing. `NodePublishVolume` defaulted the same volume to
  `nfs` and bind-mounted the empty staging directory.
- **The result:** the pod wrote successfully to the node's local disk (`/dev/sda4`)
  and nothing reached Hammerspace. Reproduced with
  `deploy-test/paypal/pod3.yaml`.

**Fix:**
- Both RPCs now resolve fsType through `nodeVolumeFsType`. The order of precedence
  is: capability, then volume context, then `nfs`. Raw block resolves to `""`.
- Publish also checks that the staging path is a mount point, and returns
  `FailedPrecondition` if it isn't. A future mismatch then fails visibly instead
  of silently writing to local disk.

**Verified:** pod3 re-run (1 NFS mount, 101 binds, data confirmed through the NFS
mount); table test for the fsType resolution.

## 3. The node no longer mounts the root export at all (dead code removed)

**Files:** `pkg/driver/node.go`, `pkg/driver/utils.go`, `pkg/driver/driver.go`,
`pkg/common/host_utils.go`, tests in `pkg/driver/node_root_test.go` and
`pkg/common/host_utils_test.go`

After change 1, `NodeStageVolume` still mounted the root export for file-backed,
block, and NFS-in-backing-share volumes. Nothing read from it: those volumes
reach their backing file through the backing share mounted at
`/tmp/<exportPath>` by `EnsureBackingShareMounted`. Staging them is now a no-op.

**Removed functions:**

| Function | Why it had no callers left |
| --- | --- |
| `EnsureRootExportMounted` | Its only caller was the root-export stage |
| `WaitForPathReady` | Only the old publish path used it |
| `NFSRootMountOptions`, `unknownMountOption`, `knownNFSMountOption` | Only `EnsureRootExportMounted` used them |
| Their unit tests | Tested removed code |

**Kept for upgrades:**
- `NodeUnstageVolume` still removes a marker and unmounts the root export after
  the last marker is gone, but only when a marker exists. Markers now come only
  from volumes staged by an older driver version.
- `acquireRootMountLock` is kept for that cleanup, with its comment updated.
- `TestRootMountLifecycleSharesOneLock` is replaced by
  `TestRootMountLockTakenOnlyForLegacyMarkers`. It checks three things:
  - staging a file-backed volume writes no marker and takes no lock;
  - unstaging without a marker takes no lock;
  - unstaging with a legacy marker does take it.

**Verified:** the `cleanup-20260924` image passed the pre-flight gate
(nfs/ext4/xfs/block) and the full block profile (64/0) on 2026-09-24. The
upgrade cleanup path (legacy markers) is covered by the unit test only.

## 4. `ESTALE` is handled like `EIO` on node paths

**Files:** `pkg/driver/node.go` (`NodeGetVolumeStats`, `NodeUnpublishVolume`,
`NodeUnstageVolume`), `pkg/driver/node_helper.go` (`publishShareBackedVolume`),
`pkg/driver/node_eio_test.go`

**Why:** an NFS mount whose server-side object is gone returns `ESTALE`, not
`EIO`.
- **Before:** only `EIO` was treated as "stale, force-detach". A stale NFS
  target failed unpublish with an error and kubelet retried it indefinitely.
- **Now:** `NodeGetVolumeStats` reports `ESTALE` as `Unavailable` (abnormal
  volume condition). Unpublish, unstage, and publish force-detach the stale
  mount (`umount -f -l`).

`TestNodeUnpublishVolumeForceUnmountsESTALEPath` covers unpublish.

## 5. Cache TTLs were always one minute

**Files:** `pkg/common/utils.go` (`SetCacheData`), new `pkg/common/utils_test.go`

**Why:** the condition was inverted (`!= 0` instead of `== 0`).
- **Caller TTLs ignored:** every caller-supplied TTL was replaced by 60s. The
  objective lists and free capacity asked for 5 minutes, and the NFS export list
  for an hour.
- **Zero meant "already expired":** a caller passing `0` got an entry that had
  expired before it could be read.

## 6. `GetCapacity` serves cluster capacity from the cache

**Files:** `pkg/driver/controller.go` (`cachedClusterAvailableCapacity`),
`pkg/client/hsclient_test.go`

**Why:** the certification test `capacity provides storage capacity information`
failed on NFS.
- **The delay:** the external provisioner refreshes `CSIStorageCapacity` for each
  StorageClass one after another, and each refresh cost a ~1.5s Anvil round trip.
  With dozens of e2e StorageClasses alive, a new class waited in the queue longer
  than the test's 60s limit.
- **The fix:** `GetCapacity` now uses the cached `FREE_CAPACITY` (5-minute TTL,
  effective only because of change 5) and queries the Anvil only when the cache
  is empty. Capacity is advisory for scheduling, so a slightly stale figure
  delivered in time is better than an exact one delivered late.
- **Deliberately not cached:** `GetClusterAvailableCapacity` itself still always
  queries the Anvil and refreshes the cache, because `CreateVolume` needs a
  current figure. The new client test pins that behaviour.

**Verified:** the capacity test passes in the 2026-09-24 full NFS run.

## 8. Nested NFS volumes get their own mount, like native NFS

**Files:** `pkg/driver/node.go` (`NodeStageVolume`, `NodePublishVolume`),
`pkg/driver/node_helper.go` (`stageNestedNFSVolume`, `nestedNFSSubPath`,
`bindMountReadOnly`, `publishShareBackedVolume`), `pkg/driver/utils.go`
(`mountShareSubPathAtBestDataportal`), `pkg/driver/controller.go`
(`ensureNFSDirectoryExists`), `pkg/common/host_utils.go`
(`RemountBindReadOnly`), `pkg/driver/node_bind_options_test.go`,
`pkg/common/host_utils_test.go`

A nested NFS volume is a directory inside a backing share (StorageClass
`fsType: nfs` plus `mountBackingShareName`).

**Why:** nested volumes were published by binding out of the node's shared
backing-share mount, which also serves every file-backed and block volume on
that share. To give each nested volume its own mount options, the driver
remounted that shared mount:
- **At publish:** a bind remount of the whole shared mount with the volume's
  flags. A volume with `ro` made the backing share read-only for everyone, so
  `qemu-img` create/resize, mkfs and metadata writes for unrelated volumes failed
  with EROFS. Each later publish also reset the flags for every volume.
- **At first mount:** the node mounted the backing share with the options of
  whichever volume got there first, and so did the controller when it created a
  nested volume's directory. The same `ro` could be baked in from the start.

Avoiding that needed a hand-kept list of which options are per-mount flags
(`isBindMountOption`), which would have to track new kernel flags.

**Fix:** the same layout other NFS CSI drivers use (csi-driver-nfs, Azure File,
CephFS): every volume gets its own NFS mount on the node, and nothing binds
volumes out of a shared mount.
- **Stage:** `NodeStageVolume` mounts the nested volume's own directory
  (`portal:/<backing share>/<volume>`) at the CSI staging path, with the volume's
  mount options, exactly as it does for a native NFS volume. The portal is chosen
  by matching the backing share's export, because the directory isn't an export
  itself.
- **Publish:** `NodePublishVolume` binds the staged mount into the pod, the same
  path native NFS uses.
- **Read-only:** a read-only publish (`req.GetReadonly()`) now makes the pod's
  bind read-only for both native and nested NFS volumes, through a private bind.
  Before, it was ignored for NFS (known issue 2 below).
- **Controller:** it mounts the backing share with no volume options when it
  creates a nested volume's directory.
- **Shared backing-share mount:** now used only by file-backed and block volumes,
  whose StorageClass `mountOptions` are by design the backing share's NFS
  options. Nothing remounts it.
- **Removed:** `publishShareBackedDirBasedVolume`, the option splitter and the
  per-mount flag list. There's no list left to maintain.

- **Own superblock (`nosharecache`):** nested volumes are mounted with
  `nosharecache` (`nestedNFSMountFlags`). Without it, the kernel NFS client
  gives mounts of the same export one shared superblock. A nested volume's
  staging mount then shows the same device and root as the backing share's mount
  and as other nested volumes on that share, and kubelet's `GetDeviceMountRefs`
  check refuses to unstage the volume while any of those stay mounted. This was
  reproduced on the cluster: a nested volume stayed staged, with its
  VolumeAttachment stuck, until the backing share was unmounted. A StorageClass
  that sets `sharecache` or `nosharecache` itself is left alone.

**Cost:** one NFS mount per nested volume per node, the same cost already
accepted for native NFS.

**Checked on the portals before writing it:** a directory inside a backing share
mounts directly over NFSv3 and v4.1, the two versions the lab portals serve.

**Verified** on the cluster with the `nosharecache-20260924` image. Two nested
volumes (one with StorageClass `ro,noatime`) and an ext4 volume ran on the same
backing share at the same time:

| Check | Result |
| --- | --- |
| Mounts | Each nested volume had its own NFS mount and superblock, separate from the backing share's (`0:477`, `0:464`, `0:478`) |
| Nested volume with StorageClass `ro` | Writes denied inside the pod |
| Other nested volume and the ext4 volume | Writable |
| Shared backing mount | Stayed `rw,relatime` |
| Read-only publish of a native NFS volume (`readOnly: true` on the claim) | Read-only in the pod, while a second pod publishing it read-write kept write access |
| Unstaging | After the pods were deleted, all three volumes unstaged and their VolumeAttachments were removed within 9 seconds, with the backing share still mounted |
| Temporary private mounts left behind | 0 |

Unit tests cover the read-only private bind sequence, how a nested volume's
directory is derived from its ID, and the `nosharecache` default.

A note on StorageClass `ro`: the container runtime (CRI-O/runc) remounts a
volume read-write inside the container unless the pod asks for read-only. `ro`
in `mountOptions` held inside the pod for a nested volume, because the volume's
own superblock is read-only. It was not enforced when the nested volume shared a
superblock with the read-write backing share, before `nosharecache`. Native NFS
with StorageClass `ro` was not tested. A read-only publish (`readOnly: true` on
the claim) was verified to be enforced.

## 9. Liveness probe waits up to 10s for the driver's Probe

**Files:** `deploy/kubernetes/kubernetes-1.36/plugin.yaml` (canonical),
`operator/internal/operator/operands.json` (regenerated with `make generate`)

**Why:** the driver's Probe logs in to the Anvil. The liveness sidecar's default
`--probe-timeout` is 1 s, so whenever the Anvil answered more slowly, a healthy
driver failed its probe and kubelet restarted it. Overnight on 2026-09-24,
while the Anvil worked through deleting 260 LUN Overflow shares, this restarted
the node plugin 51 times and the controller 112 times over about 8 hours. About
180 of the failures were `context deadline exceeded`. Each restart interrupts
in-flight mounts, which is the likely reason LUN Overflow ran out of time in
that NFS run.

**Fix:**
- The sidecar in both workloads gets `--probe-timeout=10s`.
- kubelet's `timeoutSeconds` goes from 3 to 15, so kubelet doesn't cut the
  check off before the sidecar answers.
- A failed login still fails the probe immediately; only a slow answer is
  tolerated.
- The operator must be rebuilt to pick this up, since it compiles the manifest in
  and force-applies it.

**Verified:** both live workloads carry the new settings, with the operator and
driver images `probefix-20260925`.

## 10. `DeleteVolume` succeeds for a share-backed volume that is already gone

**Files:** `pkg/driver/controller.go` (`DeleteVolume`), new
`pkg/driver/delete_volume_test.go`

**Why:** a share-backed volume whose share no longer existed was handed to the
file-backed delete path as a "legacy" case.
- **When the share's directory still existed:** the Anvil can keep it for a while
  after the share object is deleted. The path then derived a backing share of `/`
  and failed with `unable to get backing share /: <nil>` on every retry, so the
  PV was never removed. Seen after the driver restarts above: a LUN Overflow PV
  failed 37 times.
- **When the directory was gone:** the path returned success anyway, so it never
  did anything useful for these IDs.

A single-segment volume ID always names a share, and CSI requires
`DeleteVolume` to succeed for a volume that no longer exists.

**Fix:** when the share is missing, return success.

**Verified:** a unit test checks that the delete succeeds without any further
Anvil calls. On the cluster, the stuck PV was deleted within seconds of the new
controller starting.

## 7. Makefile: `GITHASH ?=` → `GITHASH =`

Your change. With `=`, the image's version label always comes from the tree
being built and can't be overridden by an inherited `GITHASH` environment
variable. Please confirm this is the intent.

## Changes tried and reverted (not in the tree)

| Change | Why it was reverted |
| --- | --- |
| NFS 4.1 step in the `MountShareAtBestDataportal` fallback | `qemu-img` byte-range locks fail with `ENOTSUPP` on the data portal's NFSv4.1, so every file-backed `CreateVolume` broke. The code comment documents this. |
| `lookupcache=pos` on NFS mounts | Passed targeted subPath runs; the next full run still failed both file-subPath tests |
| Probe tolerating several consecutive login failures | You declined it: Probe fails on the first login failure, and kubelet's `failureThreshold` provides the tolerance |
| `ClearCacheData` | Only tests used it; test-only helpers stay out of production code |

## Known issues found in review, not changed

1. **Upgrade from the root-export design (untested): documented, not changed.**
   A new pod using a volume the old driver already staged on that node is
   refused until the volume leaves that node; existing pods keep working. The
   supported path is to drain each node after updating the plugin, now
   documented in `deploy/kubernetes/README.md` ("Upgrading from 1.3.x or
   earlier"), `CHANGELOG.md` (1.4.0) and `docs/redhat-certification.md`. The
   legacy marker and root-mount cleanup in `NodeUnstageVolume` is kept for that
   drain.
2. ~~**`readOnly` is ignored for native NFS publish.**~~ Fixed by change 8.
3. **`NodeUnpublishVolume` can hang on a dead hard NFS mount.** `os.Lstat` on the
   target blocks with no timeout. A time-bounded `lstat` with force-detach was
   proposed but not implemented.
4. ~~**Livenessprobe timeout.**~~ Fixed by change 9.