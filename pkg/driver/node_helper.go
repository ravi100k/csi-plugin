package driver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"context"

	"github.com/hammer-space/csi-plugin/pkg/common"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Mount share and attach it
func (d *CSIDriver) publishShareBackedVolume(ctx context.Context, volumeId, stagingTarget, targetPath string, mountFlags []string, fqdn string) error {
	mounted, err := common.SafeIsMountPoint(targetPath)
	if err == nil && mounted {
		log.Debugf("Volume (%s) already published at %s; nothing to do", volumeId, targetPath)
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESTALE) {
			log.Warnf("native NFS target %s is stale; force-detaching before republish", targetPath)
			if unmountErr := forceUnmountTarget(targetPath); unmountErr != nil {
				return status.Errorf(codes.Internal, "clean stale NFS target %s: %v", targetPath, unmountErr)
			}
		} else {
			return status.Errorf(codes.Internal, "check NFS target %s: %v", targetPath, err)
		}
	}

	if err := os.MkdirAll(targetPath, 0755); err != nil {
		return status.Errorf(codes.Internal, "create NFS target %s: %v", targetPath, err)
	}

	if stagingTarget != "" {
		// The share is mounted once at the CSI staging path. Publishing is a local
		// bind per pod, while kubelet creates any subPath binds beneath that.
		//
		// Refuse to bind a staging path with nothing mounted on it. Binding it
		// anyway hands the pod an empty directory on the node's own disk: every
		// write succeeds, nothing reaches Hammerspace, and the data is lost when
		// the pod moves. Failing here turns that silent data loss into an error
		// kubelet retries and reports.
		staged, checkErr := common.SafeIsMountPoint(stagingTarget)
		if checkErr != nil {
			return status.Errorf(codes.Internal, "check staged NFS volume %s at %s: %v", volumeId, stagingTarget, checkErr)
		}
		if !staged {
			return status.Errorf(codes.FailedPrecondition, "NFS volume %s is not mounted at staging path %s; refusing to publish an unbacked directory", volumeId, stagingTarget)
		}
		if err := common.BindMountDevice(stagingTarget, targetPath); err != nil {
			return status.Errorf(codes.Internal, "bind staged NFS volume %s to %s: %v", stagingTarget, targetPath, err)
		}
		return nil
	}

	// Controller-side metadata operations do not have a CSI staging path.
	return d.MountShareAtBestDataportal(ctx, volumeId, targetPath, mountFlags, fqdn)
}

// Check base pv exist as backingShareName and create path with backingShareName/exportPath attach to target path
func (d *CSIDriver) publishShareBackedDirBasedVolume(ctx context.Context, backingShareName, exportPath, targetPath, fsType string, mountFlags []string, fqdn string) error {
	log.Debugf("Recived publish dir based volume request.")
	unlock, err := d.acquireVolumeLock(ctx, backingShareName)
	if err != nil {
		// surfaces to kubelet instead of hanging forever
		return err
	}
	defer unlock()

	mounted, err := common.SafeIsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			mounted = false
		} else {
			// Any other error (e.g. permission denied)
			return status.Error(codes.Internal, err.Error())
		}
	}

	if mounted {
		log.Debugf("Volume already published at %s", targetPath)
		return nil
	}

	hsVolume := &common.HSVolume{
		FQDN:       fqdn,
		FSType:     fsType,
		MountFlags: mountFlags,
	}
	log.Infof("check nfs backed volume %v", hsVolume)

	// Ensure the backing share is mounted
	if err := d.EnsureBackingShareMounted(ctx, backingShareName, hsVolume); err != nil {
		return err
	}

	// Mount the file
	log.Infof("Mounting NFS-backed volume at %s", targetPath)

	// Compute full source path inside mounted backing share
	sourceMountPoint := filepath.Join(common.ShareStagingDir, exportPath)

	// Validate that the source exists
	if _, err := os.Stat(sourceMountPoint); err != nil {
		if os.IsNotExist(err) {
			return status.Errorf(codes.NotFound, "export path %s does not exist inside share %s", exportPath, backingShareName)
		}
		return status.Errorf(codes.Internal, "error accessing source path %s: %v", sourceMountPoint, err)
	}

	if err := common.BindMountDevice(sourceMountPoint, targetPath); err != nil {
		log.Errorf("bind mount failed for %s: %v", targetPath, err)
		CleanupLoopDevice(ctx, targetPath)
		d.UnmountBackingShareIfUnused(ctx, backingShareName)
		return err
	}

	log.Infof("Successfully mounted %s -> %s", sourceMountPoint, targetPath)
	return nil
}

func (d *CSIDriver) publishFileBackedVolume(ctx context.Context, backingShareName, volumePath, targetPath, fsType string, mountFlags []string, readOnly bool, fqdn string) error {
	unlock, err := d.acquireVolumeLock(ctx, backingShareName)
	if err != nil {
		// surfaces to kubelet instead of hanging forever
		return err
	}
	defer unlock()

	log.Debug("Received publish file-backed volume request")
	mounted, err := common.SafeIsMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			// A missing kubelet target is expected on the first publish. Create it
			// with the shape required by the requested volume type.
			log.Debugf("Publish target %s does not exist; creating it", targetPath)
			if fsType != "" {
				// fsType specified => assume directory mount
				if err := os.MkdirAll(targetPath, 0755); err != nil {
					return status.Error(codes.Internal, err.Error())
				}
			} else {
				// Block volume mount => create file
				parentDir := filepath.Dir(targetPath)
				if err := os.MkdirAll(parentDir, 0755); err != nil {
					return status.Error(codes.Internal, err.Error())
				}
				f, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL, 0644)
				if err != nil {
					return status.Error(codes.Internal, err.Error())
				}
				f.Close()
			}
			mounted = false
		} else if errors.Is(err, syscall.EIO) {
			// A file-backed filesystem can remain mounted after XFS shuts itself
			// down. Do not report it as healthy or inherit it on a retry.
			log.Warnf("publish target %s returned EIO; force-detaching shut-down filesystem", targetPath)
			if unmountErr := forceUnmountTarget(targetPath); unmountErr != nil {
				return status.Errorf(codes.Internal,
					"failed to clean up publish target after I/O error: %v", unmountErr)
			}
			if fsType != "" {
				if mkdirErr := os.MkdirAll(targetPath, 0755); mkdirErr != nil {
					return status.Errorf(codes.Internal, "failed to recreate publish target after I/O error: %v", mkdirErr)
				}
			} else {
				parentDir := filepath.Dir(targetPath)
				if mkdirErr := os.MkdirAll(parentDir, 0755); mkdirErr != nil {
					return status.Error(codes.Internal, mkdirErr.Error())
				}
				f, createErr := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL, 0644)
				if createErr != nil {
					return status.Errorf(codes.Internal, "failed to recreate block publish target after I/O error: %v", createErr)
				}
				if closeErr := f.Close(); closeErr != nil {
					return status.Error(codes.Internal, closeErr.Error())
				}
			}
			mounted = false
		} else {
			// Any other error (for example permission denied) is actionable.
			log.Errorf("Failed to check whether publish target %s is a mount point: %v", targetPath, err)
			return status.Errorf(codes.Internal, "failed to inspect publish target %s: %v", targetPath, err)
		}
	}

	filePath := common.ShareStagingDir + volumePath

	if mounted {
		log.Debugf("Volume already published at %s", targetPath)
	} else {
		hsVolume := &common.HSVolume{
			FQDN:       fqdn,
			FSType:     fsType,
			MountFlags: mountFlags,
		}

		log.WithFields(log.Fields{
			"fqdn":             hsVolume.FQDN,
			"FSType":           hsVolume.FSType,
			"backingShareName": backingShareName,
		}).Info("Publish file backed volume.")

		// Ensure the backing share is mounted
		if err := d.EnsureBackingShareMounted(ctx, backingShareName, hsVolume); err != nil {
			return err
		}

		// Mount the file
		log.Infof("Mounting file-backed volume at %s", targetPath)

		if fsType == "" {
			deviceStr, err := AttachLoopDeviceWithRetry(filePath, readOnly)
			if err != nil {
				log.Errorf("failed to attach loop device: %v", err)
				CleanupLoopDevice(ctx, deviceStr)
				d.UnmountBackingShareIfUnused(ctx, backingShareName)
				return status.Errorf(codes.Internal, common.LoopDeviceAttachFailed, deviceStr, filePath)
			}
			log.Infof("File %s attached to %s", filePath, deviceStr)

			if err := common.BindMountDevice(deviceStr, targetPath); err != nil {
				log.Errorf("bind mount failed for %s: %v", deviceStr, err)
				CleanupLoopDevice(ctx, deviceStr)
				d.UnmountBackingShareIfUnused(ctx, backingShareName)
				return err
			}
		} else {
			// StorageClass mountOptions are used for the backing NFS share mount.
			// Do not pass them to the local ext4/xfs mount of the backing file.
			var filesystemMountFlags []string
			if readOnly {
				filesystemMountFlags = append(filesystemMountFlags, "ro")
			}
			if err := common.MountFilesystem(filePath, targetPath, fsType, filesystemMountFlags); err != nil {
				d.UnmountBackingShareIfUnused(ctx, backingShareName)
				return err
			}
		}
	}

	// A volume restored from a snapshot into a larger PVC has had its raw
	// backing file grown by the controller (growRestoredDeviceFile), but the
	// ext4/xfs filesystem inside it is still the snapshot's original size — only
	// a node has the loop device needed to grow it. Reconcile it here, to the
	// backing file's CURRENT size rather than to any requested size, so an
	// already-expanded volume can never be shrunk back.
	//
	// This runs on every publish, including one that found the volume already
	// mounted, because the underlying operations are idempotent no-ops when
	// there is nothing to grow. That also retries a reconciliation that failed
	// on an earlier publish attempt, which would otherwise be masked forever by
	// the "already published" path reporting success.
	//
	// KNOWN LIMITATION: skipped for read-only publishes, since resize2fs and
	// xfs_growfs must write to the filesystem. A larger restored volume whose
	// first-ever publish is read-only therefore keeps the snapshot's smaller
	// filesystem. Fixing that would require growing the filesystem through the
	// driver's own writable staging mount before ever publishing it read-only.
	if fsType != "" && !readOnly {
		if err := common.ReconcileFilesystemToBackingFile(targetPath, filePath, fsType); err != nil {
			if !mounted {
				// Fresh mount: this is where a restored volume gets the capacity
				// its PVC asked for, so a failure here must surface.
				log.Errorf("failed to reconcile filesystem size at %s: %v", targetPath, err)
				return status.Errorf(codes.Internal, "failed to reconcile filesystem size: %v", err)
			}
			// Already mounted, i.e. a republish of a healthy volume
			// (requiresRepublish drives these periodically). The reconcile is a
			// no-op in the steady state, so a transient loop-device lookup
			// failure here must not tear down a working volume -- warn and let
			// the next republish retry.
			log.Warnf("could not reconcile filesystem size at %s on republish: %v", targetPath, err)
		}
	}
	return nil
}

// NodeUnpublishVolume
func (d *CSIDriver) unpublishFileBackedVolume(ctx context.Context, volumePath, targetPath string) error {
	ctx, span := tracer.Start(ctx, "unpublishFileBackedVolume", trace.WithAttributes(
		attribute.String("volume.path", volumePath),
		attribute.String("target.path", targetPath),
	))
	defer span.End()

	//determine backing share
	backingShareName := filepath.Dir(volumePath)

	unlock, err := d.acquireVolumeLock(ctx, backingShareName)
	if err != nil {
		// surfaces to kubelet instead of hanging forever
		return err
	}
	defer unlock()

	deviceMinor, err := common.GetDeviceMinorNumber(targetPath)
	if err != nil {
		log.Errorf("could not determine corresponding device path for target path, %s, %v", targetPath, err)
		return status.Error(codes.Internal, err.Error())
	}
	lodevice := fmt.Sprintf("/dev/loop%d", deviceMinor)
	log.Infof("found device %s for mount %s", lodevice, targetPath)

	// Remove bind mount
	output, err := common.ExecCommand("umount", targetPath)
	if err != nil {
		log.Errorf("could not remove bind mount, %s", err)
		return status.Error(codes.Internal, err.Error())
	}
	log.Infof("unmounted the targetPath %s. Command output %v ", targetPath, output)
	// delete target path
	err = os.Remove(targetPath)
	if err != nil {
		log.Errorf("could not remove target path, %v", err)
		return status.Error(codes.Internal, err.Error())
	}

	// detach from loopback device
	log.Infof("detaching loop device, %s", lodevice)
	output, err = common.ExecCommand("losetup", "-d", lodevice)
	if err != nil {
		log.Errorf("%s, %v", output, err.Error())
		return status.Error(codes.Internal, err.Error())
	}

	// Unmount backing share if appropriate
	unmounted, err := d.UnmountBackingShareIfUnused(ctx, backingShareName)
	if unmounted {
		log.Infof("unmounted backing share, %s", backingShareName)
	}
	if err != nil {
		log.Errorf("unmounted backing share, %s, failed: %v", backingShareName, err)
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}
