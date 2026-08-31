# Node staging design

`NodeStageVolume` validates `staging_target_path` but intentionally does not mount
an individual volume there. The driver stages a single node-wide Hammerspace root
export and records a per-volume marker; `NodePublishVolume` then mounts or binds
the requested share, directory, loop filesystem, or block device at the kubelet
target path.

This design avoids one duplicate NFS mount per pod while preserving CSI
stage/unstage reference semantics. Marker changes and root mount transitions are
serialized, marker read errors fail closed, and the root export is unmounted only
after the last staged-volume marker is removed. The requested staging path is
never treated as the published data path.
