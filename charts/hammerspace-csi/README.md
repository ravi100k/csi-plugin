# Hammerspace CSI Helm chart

This chart installs the v1.4 controller and node components. It defaults to two
anti-affined controller replicas with both sidecar and driver Lease election.

Create `com.hammerspace.csi.credentials` in the target namespace before install;
see `deploy/kubernetes/SECRETS.md`. For an air-gapped install, override
`image.repository`, `image.tag`, or `image.digest` and mirror the CSI sidecars.

```sh
helm upgrade --install hammerspace-csi ./charts/hammerspace-csi \
  --namespace kube-system
```
