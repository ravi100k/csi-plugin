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

1. **Upgrade from the root-export design (untested).** Existing pods keep
   working. However, a *new* pod that uses a volume already staged on that node
   by the old driver will fail with `FailedPrecondition`: the staging path has
   nothing mounted, and kubelet won't re-stage an already-staged volume. It
   clears once every pod using that volume has left the node. Possible fix: when
   publishing a volume that has a legacy marker, mount the share at the staging
   path on demand.
2. **`readOnly` is ignored for native NFS publish.** `req.GetReadonly()` is passed
   only to file-backed publish, so a read-only native-NFS mount is still bound
   read-write. This predates these changes: the old bind path ignored it too.
3. **`NodeUnpublishVolume` can hang on a dead hard NFS mount.** `os.Lstat` on the
   target blocks with no timeout. A time-bounded `lstat` with force-detach was
   proposed but not implemented.
4. **Livenessprobe timeout.** The sidecar's default `--probe-timeout` of 1s is
   shorter than a Probe that has to log in to the Anvil again.
