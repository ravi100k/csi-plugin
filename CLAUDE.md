# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A CSI (Container Storage Interface) plugin, in Go, that lets Kubernetes/OpenShift
provision storage backed by a Hammerspace cluster (the "Anvil" management API).
It implements the CSI Identity, Node, and Controller services as a single binary
(`main.go` → `pkg/driver`), talking to Hammerspace over its REST API
(`pkg/client`). `operator/` is a separate Go module containing an OpenShift
Operator that deploys/manages the driver (for Red Hat certification); it is
developed and tested independently of the root module.

## Common commands

Root module (the driver):
```bash
make compile              # go build -> bin/hs-csi-plugin
make unittest              # go test ./... (excludes TestSanity)
make sanity                # functional CSI sanity tests against a real HS_ENDPOINT (test/sanity/...)
make build-dev             # build the dev/debug docker image (Dockerfile_dev)
make build                 # build the main plugin image from public dependencies
make build-release         # release build tagged with VERSION + git hash
```
Run a single test: `go test ./pkg/driver/... -run TestName -v`

Normal Docker builds require no Red Hat subscription: UBI 9 plus signed Rocky 9
packages for missing storage tools, with installed UBI packages protected.
`make runtime-base-rhel` is an optional maintainer-only alternative using
`RH_SUB_SECRET`; see README for package provenance and certification limits.

Operator module (`operator/`, separate `go.mod`, pins its own client-go — run
commands from inside `operator/`):
```bash
make test                 # go test ./... plus python3 -m unittest for hack/build_bundle.py
make build                # go build -> operator/bin/hammerspace-csi-operator
make image IMAGE=...      # docker build
```

## Architecture

### Driver (root module)

- `main.go` — env var validation, OTel setup, picks CSI v0 vs v1 server, starts
  the gRPC unix-socket listener.
- `pkg/driver/driver.go` — `CSIDriver` struct: the gRPC server plus per-volume
  and per-snapshot lock maps (`volumeLocks`/`snapshotLocks`, keyed mutex-like
  semaphores) so concurrent RPCs on the same ID serialize. Also owns
  `mountRefs`/`mountLocks`, a refcounted-mount scheme for backing shares:
  concurrent file-backed `CreateVolume` calls on the same backing share share
  one NFS mount (mounted on first reference, unmounted on last), while the
  actual mount/unmount syscalls are serialized by a per-share lock separate
  from the map-guarding lock — so one slow (~5 min) mount can't block refcount
  bookkeeping for other shares.
- `pkg/driver/controller.go` — Controller service: CreateVolume, DeleteVolume,
  snapshots, expansion, capacity. `parseVolParams` maps StorageClass params
  (`fsType`, `mountBackingShareName`, `blockBackingShareName`, `deleteDelay`,
  `objectives`, etc.) into `common.HSVolumeParameters`, which decides which of
  the three volume types (nfs/file/block) gets created.
- `pkg/driver/node.go`, `node_helper.go` — Node service: stage/publish, mount
  handling for the three volume types, loop-device setup for file/block
  volumes, volume stats, EXPAND_VOLUME.
- `pkg/driver/freezer.go` — runs `fsfreeze` inside the pod holding a volume
  during CreateSnapshot so XFS reaches a quiesce point before Anvil snapshots
  the underlying file; nil (no-op) when not running in-cluster.
- `pkg/driver/driver_csi_v0.go` — a compatibility shim exposing the same
  `CSIDriver` through the legacy CSI v0.3.0 spec, selected by
  `CSI_MAJOR_VERSION=0` (only for Kubernetes 1.10–1.12).
- `pkg/driver/utils.go` — volume ID / path helpers shared by controller and
  node code.
- `pkg/client/hsclient.go` — `HammerspaceClient`, the REST client for the Anvil
  management API (`/mgmt/v1.2/rest`), including async task polling (fast 2s
  poll, then relaxed) for long-running operations like share creation.
- `pkg/common/` — shared types (`hs_types.go`), config/constants
  (`config.go`), host-level helpers for mkfs/mount/loop-devices
  (`host_utils.go`), structured JSON logging setup, Prometheus metrics, and a
  small cache (`hs_cache.go`).

**Volume types** (selected by StorageClass `fsType` + PVC `volumeMode`, not a
direct parameter): share-backed NFS (default, `ReadWriteMany`, grows live),
file-backed (`ext4`/`xfs` file on a backing NFS share, mounted as a loop
device, single pod), and raw block (same backing-file idea, exposed as an
unformatted block device). File-backed and block volumes require a backing
share (`mountBackingShareName` / `blockBackingShareName`) and are the scalable
choice at high volume counts, since each share-backed volume is a separate
Hammerspace share/Anvil task.

Telemetry is OpenTelemetry-based and off by default; controlled via
`OTEL_TRACES_EXPORTER`/`OTEL_METRICS_EXPORTER` env vars (`none`/`console`/`otlp`,
plus `prometheus` for metrics) — see `main.go`'s `initTelemetry`.

### Operator (`operator/`)

A controller-runtime-free, dynamic-client-based reconciler for a cluster-scoped
`HammerspaceCSIDriver/cluster` CR (`operator/cmd/main.go` runs leader election +
a 15s reconcile loop).

- `internal/manifests/generate.go` derives `internal/operator/operands.json` from
  `../deploy/kubernetes/kubernetes-1.36/plugin.yaml`; run `make generate` after
  editing the canonical manifest. Builds regenerate it; tests check for drift.
- `internal/operator/render.go` — pure function: CR spec + image set →desired
  Kubernetes objects (StatefulSet, DaemonSet, RBAC, CSIDriver, SCC,
  StorageClasses, etc.) as `unstructured.Unstructured`.
- `internal/operator/reconcile.go` — `Reconciler.Reconcile`: validates the CR
  and credentials Secret, renders desired state, **refuses to adopt** any
  existing resource not already owned by this CR (`OwnershipConflict`), applies
  objects in order (RBAC/config before workloads, via server-side apply with
  `Force`), and updates `Available`/`Progressing`/`Degraded` status conditions.
  Cleanup (on deletion) removes owned workloads first, then other owned
  objects, and never touches PVCs, PVs, backend data, credentials Secrets, or
  the namespace itself — a finalizer enforces that ordering.
- `hack/build_bundle.py` — generates a local OLM bundle (CSV, CRD, annotations,
  bundle Dockerfile, file-based catalog) from the same render/RBAC inputs, for
  certification packaging; does not push to a registry or submit certification.

The operator fixes the operand namespace to `hammerspace-csi` and reads driver
credentials from an existing Secret (`com.hammerspace.csi.credentials`, keys
`username`/`password`/`endpoint`) that it does not create or copy elsewhere.

## Notes

- CSI volume/snapshot IDs and locking are per-ID (see `acquireVolumeLock`/
  `acquireSnapshotLock` in `pkg/driver/driver.go`); when touching controller or
  node RPCs, check whether an operation needs to hold one of these locks to
  stay consistent with concurrent RPCs on the same volume.
- `ext3` is intentionally unsupported (`ext4`/`xfs` only).
- Kubernetes manifests live under `deploy/kubernetes/kubernetes-<minor>/` per
  validated minor version (currently 1.29, 1.34–1.36); older/unsupported
  minors are under `deploy/kubernetes/archive/`.
