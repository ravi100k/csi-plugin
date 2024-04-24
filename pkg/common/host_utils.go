/*
Copyright 2019 Hammerspace

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	unix "golang.org/x/sys/unix"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/mount"
)

func execCommandHelper(command string, args ...string) ([]byte, error) {
	cmd := exec.Command(command, args...)
	log.Debugf("Executing command: %v", cmd)
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	if err := cmd.Start(); err != nil {
		log.Error(err)
		return nil, err
	}
	// Wait for the process to finish or kill it after a timeout (whichever happens first):
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	select {
	case <-time.After(CommandExecTimeout):
		log.Warnf("Command '%s' with args '%v' did not completed after %d seconds",
			command, args, CommandExecTimeout)
		if err := cmd.Process.Kill(); err != nil {
			log.Error("failed to kill process: ", err)
		}
		return nil, fmt.Errorf("process killed as timeout reached")
	case err := <-done:
		if err != nil {
			log.Errorf("process finished with error = %v", err)
			return nil, err
		}
	}
	return b.Bytes(), nil
}

var ExecCommand = execCommandHelper

// EnsureFreeLoopbackDeviceFile finds the next available loop device under /dev/loop*
// If no free loop devices exist, a new one is created
func EnsureFreeLoopbackDeviceFile() (uint64, error) {
	LOOP_CTL_GET_FREE := uintptr(0x4C82)
	LoopControlPath := "/dev/loop-control"
	ctrl, err := os.OpenFile(LoopControlPath, os.O_RDWR, 0660)
	if err != nil {
		return 0, fmt.Errorf("could not open %s: %v", LoopControlPath, err)
	}
	defer ctrl.Close()
	dev, _, errno := unix.Syscall(unix.SYS_IOCTL, ctrl.Fd(), LOOP_CTL_GET_FREE, 0)
	if dev < 0 {
		return 0, fmt.Errorf("could not get free device: %v", errno)
	}
	return uint64(dev), nil
}

func MountFilesystem(sourcefile, destfile, fsType string, mountFlags []string) error {
	mounter := mount.New("")
	if exists, _ := mounter.ExistsPath(destfile); !exists {
		err := os.MkdirAll(filepath.Dir(destfile), os.FileMode(0644))
		if err == nil {
			err = mounter.MakeFile(destfile)
		}
		if err != nil {
			log.Errorf("could not make destination path for mount, %v", err)
			return status.Error(codes.Internal, err.Error())
		}
	}

	err := mounter.Mount(sourcefile, destfile, fsType, mountFlags)
	if err != nil {
		if os.IsPermission(err) {
			return status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "Invalid argument") {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

func ExpandFilesystem(device, fsType string) error {
	log.Infof("Resizing filesystem on file '%s' with '%s' filesystem", device, fsType)

	var command string
	if fsType == "xfs" {
		command = "xfs_growfs"
	} else {
		command = "resize2fs"
	}
	output, err := ExecCommand(command, device)
	if err != nil {
		log.Errorf("Could not expand filesystem on device %s: %s: %s", device, err.Error(), output)
		return err
	}
	return nil
}

func BindMountDevice(sourcefile, destfile string) error {
	mounter := mount.New("")
	if exists, _ := mounter.ExistsPath(destfile); !exists {
		err := os.MkdirAll(filepath.Dir(destfile), os.FileMode(0644))
		if err == nil {
			err = mounter.MakeFile(destfile)
		}
		if err != nil {
			log.Errorf("could not make destination path for bind mount, %v", err)
			return status.Error(codes.Internal, err.Error())
		}
	}

	err := mounter.Mount(sourcefile, destfile, "", []string{"bind"})
	if err != nil {
		if os.IsPermission(err) {
			return status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

func GetDeviceMinorNumber(device string) (uint32, error) {
	s := unix.Stat_t{}
	if err := unix.Stat(device, &s); err != nil {
		return 0, err
	}
	dev := uint64(s.Rdev)
	return unix.Minor(dev), nil
}

func MakeEmptyRawFile(pathname string, size int64) error {
	log.Infof("creating file '%s'", pathname)
	sizeStr := strconv.FormatInt(size, 10)
	output, err := ExecCommand("qemu-img", "create", "-fraw", pathname, sizeStr)
	if err != nil {
		log.Errorf("%s, %v", output, err.Error())
		return err
	}
	return nil
}

func ExpandDeviceFileSize(pathname string, size int64) error {
	log.Infof("resizing device file '%s'", pathname)
	sizeStr := strconv.FormatInt(size, 10)
	loopdev, err := determineLoopDeviceFromBackingFile(pathname)
	if err != nil {
		//        log.Errorf("DFERR: loopdev: '%s', error: '%v'", loopdev, err.Error())
		return err
	}
	// Refresh the loop device size with losetup -c
	// Requires UBI image
	loresize, err := ExecCommand("losetup", "-c", loopdev)
	if err != nil {
		log.Errorf("Resizing loop device '%s' failed with output '%s': '%v'", loopdev, loresize, err.Error())
		return err
	}
	output, err := ExecCommand("qemu-img", "resize", "-fraw", pathname, sizeStr)
	if err != nil {
		log.Errorf("%s, %v", output, err.Error())
		return err
	}
	return nil
}

func FormatDevice(device, fsType string) error {
	log.Infof("formatting file '%s' with '%s' filesystem", device, fsType)
	args := []string{device}
	if fsType == "xfs" {
		args = []string{"-m", "reflink=0", device}
	}
	output, err := ExecCommand(fmt.Sprintf("mkfs.%s", fsType), args...)
	if err != nil {
		log.Info(err)
		if output != nil && strings.Contains(string(output), "will not make a filesystem here") {
			log.Warningf("Device %s is already mounted", device)
			return err
		}
		log.Errorf("Could not format device %s: %s", device, err.Error())
		return err
	}
	return nil
}

func DeleteFile(pathname string) error {
	log.Infof("deleting file '%s'", pathname)
	err := os.Remove(pathname)
	if err != nil {
		return err
	}

	return nil
}

func MountShare(sourcePath, targetPath string, mountFlags []string) error {
	log.Infof("mounting %s to %s, with options %v", sourcePath, targetPath, mountFlags)
	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return status.Error(codes.Internal, err.Error())
		}
	}

	if !notMnt {
		return nil
	}

	mo := mountFlags

	mounter := mount.New("")
	err = mounter.Mount(sourcePath, targetPath, "nfs", mo)
	if err != nil {
		if os.IsPermission(err) {
			return status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		return status.Error(codes.Internal, err.Error())
	}

	return nil
}

func determineBackingFileFromLoopDevice(lodevice string) (string, error) {
	output, err := ExecCommand("losetup", "-a")
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"could not determine backing file for loop device, %v", err)
	}
	devices := strings.Split(string(output), "\n")
	for _, d := range devices {
		if d != "" {
			device := strings.Split(d, " ")
			if lodevice == strings.Trim(device[0], ":()") {
				return strings.Trim(device[len(device)-1], ":()"), nil
			}
		}
	}
	return "", status.Errorf(codes.Internal,
		"could not determine backing file for loop device")
}

// Note that this function does not work in Alpine image due to
// losetup cutting the output off at 79 characters
func determineLoopDeviceFromBackingFile(backingfile string) (string, error) {
	log.Infof("determine loop device from backing file: '%s'", backingfile)
	output, err := ExecCommand("losetup", "-a")
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"could not determine loop device for backing file, %v", err)
	}
	devices := strings.Split(string(output), "\n")
	for _, d := range devices {
		if d != "" {
			device := strings.Split(d, " ")
			if backingfile == strings.Trim(device[2], ":()") {
				log.Infof("matched loop dev: '%s'", strings.Trim(device[0], ":()"))
				return strings.Trim(device[0], ":()"), nil
			}
		}
	}
	return "", status.Errorf(codes.Internal,
		"could not determine loop device for backing file")
}

func GetNFSExports(address string) ([]string, error) {
	output, err := ExecCommand("showmount", "--no-headers", "-e", address)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"could not determine nfs exports, %v: %s", err, output)
	}
	exports := strings.Split(string(output), "\n")
	toReturn := []string{}
	for _, export := range exports {
		exportTokens := strings.Fields(export)
		if len(exportTokens) > 0 {
			toReturn = append(toReturn, exportTokens[0])
		}
	}
	if len(toReturn) == 0 {
		return nil, status.Errorf(codes.Internal,
			"could not determine nfs exports, command output: %s", output)
	}
	return toReturn, nil
}

func CheckNFSExports(ctx context.Context, address string) (bool, error) {
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("timeout reached while checking NFS exports for %s", address)
	default:
		// rcpinfo -a uaddr -T tcp6 100003 3
		// rpcinfo -a 10.200.104.82.8.1 -T tcp 100003 3 -> program 100003 version 3 ready and waiting
		protocol := "tcp"
		if strings.Contains(address, ":") {
			protocol = "tcp6"
		}
		uaddr, err := computeUaddr(address, 2049)
		if err != nil {
			return false, err
		}
		output, err := ExecCommand("rpcinfo", "-a", uaddr, "-T", protocol, "100003", "3")
		if err != nil {
			return false, status.Errorf(codes.Internal,
				"could not determine nfs avalibility, %v: %s", err, output)
		}
		log.Infof("rpcinfo %v", output)
		return true, nil
	}
}

func computeUaddr(ipAddress string, port int) (string, error) {
	ipType, err := checkIPType(ipAddress)
	switch ipType {
	case "IPv4":
		log.Infof("got ipv4 IP while computing uaddr ip - %s:%d", ipAddress, port)
		return computeIPv4Uaddr(ipAddress, port), nil
	case "IPv6":
		log.Infof("got ipv6 IP while computing uaddr ip - %s:%d", ipAddress, port)
		return computeIPv6Uaddr(ipAddress, port), nil
	default:
		log.Infof("Invalid ip while computing uaddr ip - %s:%d", ipAddress, port)
		return "", err
	}
}

func computeIPv4Uaddr(ipAddress string, port int) string {
	// Split the IPv4 address into octets
	octets := strings.Split(ipAddress, ".")

	if len(octets) != 4 {
		return ""
	}

	// Convert port to hexadecimal and get the last two digits
	portHex := strconv.FormatInt(int64(port), 16)
	portHex = fmt.Sprintf("%04s", portHex) // pad with zeros if necessary
	portHigh, _ := strconv.ParseInt(portHex[:2], 16, 0)
	portLow, _ := strconv.ParseInt(portHex[2:], 16, 0)

	// Compute the final uaddr string for IPv4
	uaddr := fmt.Sprintf("%s.%d.%d", ipAddress, portHigh, portLow)
	return uaddr
}

func computeIPv6Uaddr(ipAddress string, port int) string {
	// Convert port to hexadecimal and format it
	portHex := fmt.Sprintf("%04x", port)

	// Compute the final uaddr string for IPv6
	uaddr := fmt.Sprintf("[%s]:%s", ipAddress, portHex)
	return uaddr
}

func checkIPType(ipAddress string) (string, error) {
	ip := net.ParseIP(ipAddress)
	if ip == nil {
		return "", errors.New("invalid IP address")
	}
	if ip.To4() != nil {
		return "IPv4", nil
	} else if ip.To16() != nil {
		return "IPv6", nil
	}
	return "", errors.New("unknown IP type")
}

func IsShareMounted(targetPath string) (bool, error) {
	notMnt, err := mount.IsNotMountPoint(mount.New(""), targetPath)

	if err != nil {
		if os.IsNotExist(err) {
			return false, status.Error(codes.NotFound, EmptyTargetPath)
		} else {
			return false, status.Error(codes.Internal, err.Error())
		}
	}
	if notMnt {
		return false, nil
	}
	return true, nil
}

func UnmountFilesystem(targetPath string) error {
	mounter := mount.New("")

	isMounted, err := IsShareMounted(targetPath)

	if err != nil {
		log.Error(err.Error())
		return status.Error(codes.Internal, err.Error())
	}
	if !isMounted {
		return nil
	}

	err = mounter.Unmount(targetPath)
	if err != nil {
		log.Error(err.Error())
		return status.Error(codes.Internal, err.Error())
	}
	// delete target path
	err = os.Remove(targetPath)
	if err != nil {
		log.Errorf("could not remove target path, %v", err)
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

func SetMetadataTags(localPath string, tags map[string]string) error {
	// hs attribute set localpath -e "CSI_DETAILS_TABLE{'<version-string>','<plugin-name-string>','<plugin-version-string>','<plugin-git-hash-string>'}"
	_, err := ExecCommand("hs",
		"attribute",
		"set", "CSI_DETAILS",
		fmt.Sprintf("-e \"CSI_DETAILS_TABLE{'%s','%s','%s','%s'}\"", CsiVersion, CsiPluginName, Version, Githash),
		localPath)
	if err != nil {
		log.Warn("Failed to set CSI_DETAILS metadata " + err.Error())
	}

	for tag_key, tag_value := range tags {
		output, err := ExecCommand("hs",
			"-v", "tag",
			"set", "-e", fmt.Sprintf("'%s'", tag_value), tag_key, localPath,
		)

		// FIXME: The HS client returns exit code 0 even on failure, so we can't detect errors
		if err != nil {
			log.Error("Failed to set tag " + err.Error())
			break
		}
		log.Debugf("HS command output: %s", output)
	}

	return err
}
