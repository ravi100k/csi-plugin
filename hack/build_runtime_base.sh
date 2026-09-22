#!/usr/bin/env bash
# Maintainer-only alternative to the public Dockerfile runtime stage.
# Builds and locally tags the Hammerspace CSI runtime base image: UBI9 plus
# the OS packages that require a real RHEL subscription and are not present
# in UBI's free repos (nfs-utils, xfsprogs, e2fsprogs, gssproxy, qemu-img,
# quota, rpcbind, libverto-libevent, libnfsidmap and dependency libraries).
#
# Uses `docker run`/`exec`/`commit` rather than `docker build --secret`
# because on at least one build host, BuildKit's build sandbox fails any
# thread-creating syscall (subscription-manager's registration, glibc's
# async DNS resolution) that a plain container does not hit.
#
# Usage: RH_SUB_SECRET=~/.config/redhat/build-subscription.env hack/build_runtime_base.sh
set -euo pipefail

BASE_IMAGE="registry.access.redhat.com/ubi9/ubi:9.6"
TAG="${RUNTIME_BASE_TAG:-hammerspaceinc/csi-plugin-runtime-base:ubi9-9.6-rhel}"
SECRET_FILE="${RH_SUB_SECRET:-$HOME/.config/redhat/build-subscription.env}"
CONTAINER_NAME="hs-csi-runtime-base-build-$$"

if [[ ! -r "$SECRET_FILE" ]]; then
  echo "Secret file not found/readable: $SECRET_FILE" >&2
  echo "Set RH_ORG_ID and RH_ACTIVATION_KEY in that file (see README)." >&2
  exit 1
fi

cleanup() { docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --name "$CONTAINER_NAME" \
  -v "$SECRET_FILE:/run/secrets/rhsub:ro" \
  "$BASE_IMAGE" sleep infinity >/dev/null

# The container-side trap guarantees unregister/cleanup even if the dnf
# install fails partway, so a transient failure can't leave an orphaned
# registered system consuming a seat on the subscription's node pool.
docker exec "$CONTAINER_NAME" bash -c '
  set -a; . /run/secrets/rhsub; set +a
  trap "subscription-manager unregister || true; subscription-manager clean || true; \
        rm -rf /etc/pki/entitlement /etc/pki/consumer/*.pem /var/lib/rhsm/*" EXIT
  set -e
  subscription-manager register --org="$RH_ORG_ID" --activationkey="$RH_ACTIVATION_KEY"
  dnf --nodocs --nobest -y install \
      util-linux \
      python3-pip \
      libcom_err-devel \
      ca-certificates \
      e2fsprogs \
      e2fsprogs-libs \
      gssproxy \
      keyutils-libs \
      keyutils \
      libbasicobjects \
      libcollection \
      libini_config \
      libnfsidmap \
      nfs-utils \
      libref_array \
      libverto-libev \
      qemu-img \
      quota \
      quota-nls \
      rpcbind \
      xfsprogs
  python3 -m pip install --no-cache-dir --user hstk
  mkdir -p /licenses
  ln -s /usr/share/licenses /licenses/rpm-packages
  dnf clean all
  rm -rf /var/cache/dnf
'

docker commit --change 'CMD []' "$CONTAINER_NAME" "$TAG" >/dev/null
echo "Built ${TAG}"
