package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/hammer-space/csi-plugin/pkg/common"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Every entry point that mounts, unmounts, or binds off the node-wide root
// export must contend on one lock, or an unstage can tear the export down
// underneath a volume that is already published on it -- the ESTALE race that
// broke subPath. Holding the lock externally and checking each RPC gives up on
// it proves they all take the same one; none of them can reach real mount
// syscalls here, because none of them get past the lock.
func TestRootMountLifecycleSharesOneLock(t *testing.T) {
	d := &CSIDriver{volumeLocks: make(map[string]*keyLock)}
	unlock, err := d.acquireRootMountLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// Already-cancelled, so a contending caller gives up immediately instead of
	// waiting out the lock's full timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	capability := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "nfs"}},
	}

	_, err = d.NodeStageVolume(ctx, &csi.NodeStageVolumeRequest{
		VolumeId: "/share", StagingTargetPath: "/unused", VolumeCapability: capability,
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("NodeStageVolume did not take the root mount lock: %v", err)
	}

	_, err = d.NodeUnstageVolume(ctx, &csi.NodeUnstageVolumeRequest{
		VolumeId: "/share", StagingTargetPath: "/unused",
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("NodeUnstageVolume did not take the root mount lock: %v", err)
	}

	if err := d.publishShareBackedVolume(ctx, "/share", "/unused", nil, ""); status.Code(err) != codes.Aborted {
		t.Fatalf("publishShareBackedVolume did not take the root mount lock: %v", err)
	}
}

// The root mount lock is keyed by a host path, so it must not collide with a
// CSI volume ID, which is always a Hammerspace share or file path.
func TestRootMountLockKeyIsNotAVolumeID(t *testing.T) {
	d := &CSIDriver{volumeLocks: make(map[string]*keyLock)}
	unlock, err := d.acquireRootMountLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// A real volume must still be lockable while the root lock is held.
	volumeUnlock, err := d.acquireVolumeLock(context.Background(), "/"+common.SharePathPrefix+"pvc-test")
	if err != nil {
		t.Fatalf("a volume lock should not contend with the root mount lock: %v", err)
	}
	volumeUnlock()
}
