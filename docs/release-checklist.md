# v1.4.0 release checklist

- Run formatting, `go vet`, unit tests, race tests, mock-Anvil tests, and current
  CSI sanity.
- Run the Kubernetes E2E matrix in `docs/v1.4.0-action-plan.md` on every supported
  Kubernetes minor and filesystem mode.
- Exercise leader termination during create, delete, expand, clone, and snapshot.
- Test controller death during freeze and file formatting.
- Build with `--pull --no-cache`; reject fixable HIGH/CRITICAL Trivy findings.
- Generate the SPDX SBOM and provenance, sign the immutable image digest, and
  publish checksums.
- Qualify v1.3-to-v1.4 upgrade and rollback with existing PVCs continuously in use.
- Record Hammerspace/Anvil, Rocky Linux, NFS, scale, and performance results in
  the release candidate report before GA approval.
