#!/usr/bin/env python3
"""Generate an OLM bundle and file-based catalog from the deployment inputs.

Requires PyYAML. This creates local files only; it never publishes images.
"""
import argparse
import copy
import json
from pathlib import Path
import re

import yaml

ROOT = Path(__file__).resolve().parents[1]
KEYS = {"driver", "provisioner", "attacher", "snapshotter", "resizer", "registrar", "livenessprobe"}


def generate(args):
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?", args.version):
        raise ValueError("version must be a semantic version")
    images = json.loads(args.images.read_text())
    if set(images) != KEYS or not all(isinstance(v, str) and v for v in images.values()):
        raise ValueError("images must contain exactly the six nonempty operand image references")
    if args.release:
        if not args.openshift_versions:
            raise ValueError("release requires an explicitly validated OpenShift version range")
        for image in [args.operator_image, *images.values()]:
            if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image):
                raise ValueError("release Operator and operand images must use sha256 digests")
    deployment = next(d for d in yaml.safe_load_all((ROOT / "config/manager.yaml").read_text()) if d["kind"] == "Deployment")
    container = deployment["spec"]["template"]["spec"]["containers"][0]
    container["image"] = args.operator_image
    for env in container["env"]:
        if env["name"].startswith("RELATED_IMAGE_"):
            env["value"] = images[env["name"][len("RELATED_IMAGE_"):].lower()]
    rbac = list(yaml.safe_load_all((ROOT / "config/rbac.yaml").read_text()))
    cluster_rules = next(d["rules"] for d in rbac if d["kind"] == "ClusterRole")
    rules = next(d["rules"] for d in rbac if d["kind"] == "Role")
    example = yaml.safe_load((ROOT / "examples/driver.yaml").read_text())
    name = "hammerspace-csi-operator.v" + args.version
    annotations = {
        "capabilities": "Basic Install",
        "categories": "Storage",
        "containerImage": args.operator_image,
        "operatorframework.io/suggested-namespace": "hammerspace-csi",
        "alm-examples": json.dumps([example]),
    }
    if args.openshift_versions:
        annotations["com.redhat.openshift.versions"] = args.openshift_versions
    csv = {
        "apiVersion": "operators.coreos.com/v1alpha1", "kind": "ClusterServiceVersion",
        "metadata": {"name": name, "annotations": annotations},
        "spec": {
            "displayName": "Hammerspace CSI Operator",
            "description": "Manages Hammerspace CSI workloads for NFS, raw block, and ext4/xfs file-backed volumes. Install in hammerspace-csi. Requires an existing credentials Secret and Linux nodes with NFS and loop-device support. Development packaging; certification is a separate release gate.",
            "version": args.version, "maturity": "alpha", "provider": {"name": "Hammerspace"},
            "installModes": [{"type": mode, "supported": mode == "OwnNamespace"} for mode in ["OwnNamespace", "SingleNamespace", "MultiNamespace", "AllNamespaces"]],
            "customresourcedefinitions": {"owned": [{"name": "hammerspacecsidrivers.storage.hammerspace.com", "version": "v1alpha1", "kind": "HammerspaceCSIDriver", "displayName": "Hammerspace CSI Driver", "description": "Singleton configuration for the Hammerspace CSI installation"}]},
            "install": {"strategy": "deployment", "spec": {
                "deployments": [{"name": deployment["metadata"]["name"], "spec": copy.deepcopy(deployment["spec"])}],
                "permissions": [{"serviceAccountName": "hammerspace-csi-operator", "rules": rules}],
                "clusterPermissions": [{"serviceAccountName": "hammerspace-csi-operator", "rules": cluster_rules}],
            }},
            "relatedImages": [{"name": "operator", "image": args.operator_image}] + [{"name": k, "image": v} for k, v in sorted(images.items())],
        },
    }
    out = args.output
    (out / "manifests").mkdir(parents=True, exist_ok=True)
    (out / "metadata").mkdir(exist_ok=True)
    (out / "manifests/hammerspace-csi-operator.clusterserviceversion.yaml").write_text(yaml.safe_dump(csv, sort_keys=False))
    (out / "manifests/hammerspacecsidrivers.crd.yaml").write_text((ROOT / "config/crd.yaml").read_text())
    bundle_annotations = {
        "operators.operatorframework.io.bundle.mediatype.v1": "registry+v1",
        "operators.operatorframework.io.bundle.manifests.v1": "manifests/",
        "operators.operatorframework.io.bundle.metadata.v1": "metadata/",
        "operators.operatorframework.io.bundle.package.v1": "hammerspace-csi-operator",
        "operators.operatorframework.io.bundle.channels.v1": "alpha",
        "operators.operatorframework.io.bundle.channel.default.v1": "alpha",
    }
    (out / "metadata/annotations.yaml").write_text(yaml.safe_dump({"annotations": bundle_annotations}, sort_keys=False))
    labels = "\n".join("LABEL " + json.dumps(k) + "=" + json.dumps(v) for k, v in bundle_annotations.items())
    (out / "Dockerfile").write_text("FROM scratch\n" + labels + "\nCOPY manifests /manifests/\nCOPY metadata /metadata/\n")
    catalog = [
        {"schema": "olm.package", "name": "hammerspace-csi-operator", "defaultChannel": "alpha"},
        {"schema": "olm.channel", "package": "hammerspace-csi-operator", "name": "alpha", "entries": [{"name": name}]},
        {"schema": "olm.bundle", "package": "hammerspace-csi-operator", "name": name, "image": args.bundle_image,
         "properties": [{"type": "olm.package", "value": {"packageName": "hammerspace-csi-operator", "version": args.version}},
                        {"type": "olm.gvk", "value": {"group": "storage.hammerspace.com", "version": "v1alpha1", "kind": "HammerspaceCSIDriver"}}],
         "relatedImages": csv["spec"]["relatedImages"]},
    ]
    (out / "catalog.json").write_text("\n".join(json.dumps(item) for item in catalog) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--operator-image", required=True)
    parser.add_argument("--bundle-image", required=True)
    parser.add_argument("--version", default=(ROOT / "VERSION").read_text().strip())
    parser.add_argument("--images", type=Path, default=ROOT / "config/development-images.json")
    parser.add_argument("--output", type=Path, default=ROOT / "bundle")
    parser.add_argument("--openshift-versions")
    parser.add_argument("--release", action="store_true", help="require digest pins and explicit OpenShift range; does not imply certification")
    args = parser.parse_args()
    try:
        generate(args)
    except (ValueError, OSError) as exc:
        parser.error(str(exc))


if __name__ == "__main__":
    main()
