# Red Hat OpenShift CSI release gate

Development images and manifests are intentionally not certification artifacts.
Use this gate for every image and OpenShift minor release submitted to Red Hat.

## 1. Build the driver from Red Hat packages

Build the release image from the public UBI-based runtime. The installer protects
the original UBI packages from replacement or modification and records the
provenance of the additional signed Rocky storage-tool RPMs. The release verifier
prints those packages for inclusion in certification review.

```sh
make build-release RELEASE=1 BUILD_FLAGS='--no-cache'
```

The driver deliberately runs as root because it performs NFS mounts, loop-device
attachment, filesystem formatting and growth. Record privileged host access in
the certification project. The same requirement and the dedicated operand SCC
must be described to customers.

## 2. Certify and pin every image

Push the driver and Operator images, run Red Hat preflight against their immutable
digests, and resolve every failure. The Red Hat Certification Service performs
the authoritative vulnerability check.

CSI sidecars are operands too. Red Hat-maintained OpenShift payload sidecars are
an option, but are not imposed by this build. Digest-pinned upstream
`registry.k8s.io/sig-storage` sidecars are also permitted as release inputs when
they are declared in `spec.relatedImages`, tested with the target OpenShift
release, and accepted in the certification project. Existing certification of
another product is useful precedent, not transferable approval for this bundle.

Copy `operator/config/certified-images.example.json`, replace every placeholder
with an approved digest, and generate the release bundle:

```sh
python3 operator/hack/build_bundle.py \
  --release \
  --version 1.4.0 \
  --openshift-versions '=v4.22' \
  --operator-image PARTNER_REGISTRY/hammerspace-csi-operator@sha256:DIGEST \
  --bundle-image PARTNER_REGISTRY/hammerspace-csi-operator-bundle:1.4.0 \
  --images /secure/path/certified-images.json
```

Release generation requires digest-pinned Operator and operand images, including
every sidecar. It writes the required CSV support, feature, subscription,
creation-date, CSI and OpenShift-version annotations.

## UBI 9 certification rationale

The release driver is built from Red Hat UBI 9 and remains identifiable as a UBI
derivative through its inherited Red Hat labels and RPM database. Storage tools
that are unavailable in the public UBI repositories are installed from signed
Rocky Linux 9 repositories without replacing installed UBI packages. Those
repositories are disabled afterward, package provenance is retained, and the
added licenses are available in the image. Release verification rejects kernel
packages, missing licenses, missing required labels, and 40 or more layers.

This preserves the UBI foundation and does not replace or modify Red Hat package
files. The additional community RPMs are not represented as Red Hat-supported
content; their provenance must be disclosed and accepted during certification
review. Application and dependency licenses are exposed under `/licenses`.

The CSI driver declares `USER 0` because NFS mounting, loop-device management,
filesystem formatting, freezing and online growth require host-level privilege.
This is not a general-purpose root requirement: only the controller and node
driver service accounts receive the dedicated, scoped SCC, while the Operator
runs as a non-root user without added capabilities. Record the privileged-image
exception in Partner Connect and use this same configuration for certification
testing and customer documentation.

## 3. Validate packaging and behavior

Run all Operator SDK validation suites and `opm validate`. Install through OLM on
a clean targeted OpenShift cluster. Exercise install, update, rollback where
supported, uninstall and reinstall while verifying existing mounts and data.

Run separate OpenShift CSI profiles for every claimed protocol and architecture.
The capability manifest must match measured behavior. Native NFS snapshot
creation, listing, and deletion are supported, but do not advertise native NFS
snapshot restore while `shareBackedSnapshotRestoreSupported` is false. Submit
only runs with no blocking failures.

Document supported OpenShift/RHCOS/RHEL versions, node prerequisites, privileged
access, unsupported OpenShift Virtualization storage features, and differences
between patch, minor and major upgrades. Repeat certification for each required
OpenShift minor release and rebuild promptly for security updates.
