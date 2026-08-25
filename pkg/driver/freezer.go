/*
Copyright 2019 Hammerspace
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Freezer implements the workload-quiesce step for consistent snapshots.
//
// When the driver receives a CreateSnapshot RPC for a file-backed volume, the
// bytes that the Anvil sees are whatever has already been flushed by the
// worker's NFS client. In-flight XFS pagecache, uncommitted XFS log
// transactions, and NFS write-back all live on the worker and can silently
// leave the on-disk state inconsistent at snapshot time. Loop-remount of that
// file on restore runs XFS log recovery, which for a dirty log can roll back
// legitimate transactions and produce an empty filesystem.
//
// The fix is to freeze the filesystem inside the pod that holds it open,
// so XFS quiesces (log flushed, no in-flight transactions) before Anvil takes
// the snapshot. This is the same approach Velero pre-hooks / Kanister
// blueprints take, but done inside the driver so the user doesn't have to
// annotate anything.
package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// FrozenTarget describes one csi-node pod + node-side mount path that was
// frozen and needs to be unfrozen. We deliberately do NOT exec inside the
// user's app pod because we can't assume its image ships `fsfreeze`
// (nginx-alpine, distroless, scratch, etc. don't). Instead we find the
// csi-node DaemonSet pod on the same node — the driver's own image is
// guaranteed to have `fsfreeze` from util-linux — and freeze the mount via
// its shared /var/lib/kubelet propagation.
type FrozenTarget struct {
	Namespace string `json:"namespace"` // kube-system, where csi-node lives
	PodName   string `json:"podName"`   // csi-node-XXXXX
	Container string `json:"container"` // hs-csi-plugin-node
	MountPath string `json:"mountPath"` // kubelet-managed CSI mount
	NodeName  string `json:"nodeName"`  // used to resolve a replacement csi-node pod
	// For diagnostics only:
	UserPodNs   string `json:"userPodNamespace"`
	UserPodName string `json:"userPodName"`
}

// Freezer holds the kube client + REST config needed to exec into pods.
type Freezer struct {
	clientset kubernetes.Interface
	restCfg   *rest.Config
	state     frozenTargetStore
	exec      func(context.Context, FrozenTarget, string) error
}

// NewFreezer builds a Freezer from the pod's in-cluster credentials. If the
// driver isn't running in-cluster (e.g. local dev), returns nil so callers
// can no-op through.
func NewFreezer() *Freezer {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Warnf("Freezer: not running in-cluster (%v); consistency-freeze disabled", err)
		return nil
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Warnf("Freezer: kubernetes.NewForConfig failed (%v); consistency-freeze disabled", err)
		return nil
	}
	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "kube-system"
	}
	f := &Freezer{clientset: cs, restCfg: cfg}
	f.state = &kubeFrozenTargetStore{clientset: cs, namespace: namespace}
	f.exec = f.execFsfreeze
	return f
}

// FreezeForVolumeHandle locates every running Pod that has the CSI volume
// (identified by volumeHandle) mounted, then execs `fsfreeze --freeze <path>`
// inside the first container of each such Pod. Returns the list of targets
// that were successfully frozen — pass those to Unfreeze after the snapshot.
//
// Failure to freeze is best-effort by design: the snapshot proceeds even if
// freeze fails on some/all pods, matching Velero's default behavior. Callers
// should log but not fail on freeze errors.
func (f *Freezer) FreezeForVolumeHandle(ctx context.Context, volumeHandle string) []FrozenTarget {
	if f == nil {
		return nil
	}
	targets, err := f.findMountsForVolumeHandle(ctx, volumeHandle)
	if err != nil {
		log.Warnf("Freezer: findMountsForVolumeHandle(%s) failed: %v", volumeHandle, err)
		return nil
	}
	if len(targets) == 0 {
		log.Infof("Freezer: no running pods hold volumeHandle=%s; nothing to freeze", volumeHandle)
		return nil
	}
	var frozen []FrozenTarget
	for _, t := range targets {
		if f.state == nil {
			log.Errorf("Freezer: no persistent freeze-state store; refusing to freeze %s", t.MountPath)
			continue
		}
		// Persist intent BEFORE freezing. This closes the fatal crash window
		// between a successful fsfreeze and writing its recovery record. A crash
		// at any point after Save leaves a target for the next leader to thaw.
		if err := f.state.Save(context.WithoutCancel(ctx), t); err != nil {
			log.Errorf("Freezer: persist freeze intent for %s failed: %v; refusing to freeze", t.MountPath, err)
			continue
		}
		if err := f.runFsfreeze(ctx, t, "--freeze"); err != nil {
			log.Warnf("Freezer: fsfreeze --freeze via %s/%s for %s/%s (path=%s) FAILED: %v",
				t.Namespace, t.PodName, t.UserPodNs, t.UserPodName, t.MountPath, err)
			if deleteErr := f.state.Delete(context.WithoutCancel(ctx), t); deleteErr != nil {
				log.Errorf("Freezer: remove failed freeze intent for %s: %v", t.MountPath, deleteErr)
			}
			continue
		}
		log.Infof("Freezer: fsfreeze --freeze via %s/%s for %s/%s (path=%s) OK and persisted",
			t.Namespace, t.PodName, t.UserPodNs, t.UserPodName, t.MountPath)
		frozen = append(frozen, t)
	}
	return frozen
}

// Unfreeze runs `fsfreeze --unfreeze` for every target that FreezeForVolumeHandle
// successfully froze. Reverse order so lower layers unblock first.
func (f *Freezer) Unfreeze(ctx context.Context, frozen []FrozenTarget) {
	if f == nil {
		return
	}
	for i := len(frozen) - 1; i >= 0; i-- {
		t := frozen[i]
		if err := f.runFsfreeze(ctx, t, "--unfreeze"); err != nil {
			log.Errorf("Freezer: fsfreeze --unfreeze via %s/%s for %s/%s (path=%s) FAILED — filesystem may remain frozen; investigate: %v",
				t.Namespace, t.PodName, t.UserPodNs, t.UserPodName, t.MountPath, err)
			continue
		}
		log.Infof("Freezer: fsfreeze --unfreeze via %s/%s for %s/%s (path=%s) OK",
			t.Namespace, t.PodName, t.UserPodNs, t.UserPodName, t.MountPath)
		if f.state != nil {
			if err := f.state.Delete(context.WithoutCancel(ctx), t); err != nil {
				log.Errorf("Freezer: remove recovered state for %s failed: %v", t.MountPath, err)
			}
		}
	}
}

func (f *Freezer) runFsfreeze(ctx context.Context, target FrozenTarget, op string) error {
	if f.exec != nil {
		return f.exec(ctx, target, op)
	}
	return f.execFsfreeze(ctx, target, op)
}

// RecoverFrozenTargets thaws every target left in the cluster-persistent state
// store by a crashed controller. Successful entries are removed; failures stay
// recorded for the next leadership/startup reconciliation.
func (f *Freezer) RecoverFrozenTargets(ctx context.Context) error {
	if f == nil || f.state == nil {
		return nil
	}
	targets, err := f.state.List(ctx)
	if err != nil {
		return fmt.Errorf("list persisted frozen targets: %w", err)
	}
	var recoveryErrors []error
	for _, target := range targets {
		if target.NodeName != "" {
			pod, container, resolveErr := f.findCsiNodePodOnNode(ctx, target.NodeName)
			if resolveErr != nil {
				recoveryErrors = append(recoveryErrors, fmt.Errorf("resolve csi-node for %s: %w", target.MountPath, resolveErr))
				continue
			}
			target.Namespace, target.PodName, target.Container = pod.Namespace, pod.Name, container
		}
		if err := f.runFsfreeze(ctx, target, "--unfreeze"); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("unfreeze %s: %w", target.MountPath, err))
			continue
		}
		if err := f.state.Delete(ctx, target); err != nil {
			recoveryErrors = append(recoveryErrors, fmt.Errorf("delete state for %s: %w", target.MountPath, err))
		}
	}
	return errors.Join(recoveryErrors...)
}

// findMountsForVolumeHandle returns every (csi-node-pod, mountPath) tuple
// covering the driver's node-side mount of the given CSI volume. See
// FrozenTarget for why we target csi-node instead of the user's app pod.
//
// The lookup chain is:
//  1. PV whose spec.csi.volumeHandle matches → its ClaimRef → PV name
//  2. Running pods that reference that PVC → their node names + pod UIDs
//  3. For each such node, the csi-node DaemonSet pod running there
//  4. The kubelet-managed mount path for this volume in that user pod
func (f *Freezer) findMountsForVolumeHandle(ctx context.Context, volumeHandle string) ([]FrozenTarget, error) {
	// Step 1: PV with matching csi.volumeHandle → its claim ref
	pvs, err := f.clientset.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list PVs: %w", err)
	}
	var claimNs, claimName, pvName string
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle != volumeHandle {
			continue
		}
		if pv.Spec.ClaimRef == nil {
			continue
		}
		claimNs = pv.Spec.ClaimRef.Namespace
		claimName = pv.Spec.ClaimRef.Name
		pvName = pv.Name
		break
	}
	if claimName == "" {
		return nil, nil
	}

	// Step 2: Running pods in that namespace that reference the PVC
	pods, err := f.clientset.CoreV1().Pods(claimNs).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", claimNs, err)
	}

	// Step 3: For each such pod, find the csi-node pod on the same node
	// and compute the kubelet-managed mount path.
	var targets []FrozenTarget
	for i := range pods.Items {
		userPod := &pods.Items[i]
		if userPod.Status.Phase != corev1.PodRunning {
			continue
		}
		// Verify at least one PVC volume in this pod references our claim
		found := false
		for _, v := range userPod.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claimName {
				found = true
				break
			}
		}
		if !found {
			continue
		}

		// csi-node pod on this user pod's node
		nodeName := userPod.Spec.NodeName
		if nodeName == "" {
			continue
		}
		csiNodePod, csiNodeContainer, err := f.findCsiNodePodOnNode(ctx, nodeName)
		if err != nil {
			log.Warnf("Freezer: no csi-node pod on node %s: %v", nodeName, err)
			continue
		}

		// kubelet's per-pod CSI mount path — csi-node has this under
		// mountPropagation=Bidirectional on /var/lib/kubelet.
		mountPath := fmt.Sprintf(
			"/var/lib/kubelet/pods/%s/volumes/kubernetes.io~csi/%s/mount",
			userPod.UID, pvName,
		)

		targets = append(targets, FrozenTarget{
			Namespace:   csiNodePod.Namespace,
			PodName:     csiNodePod.Name,
			Container:   csiNodeContainer,
			MountPath:   mountPath,
			NodeName:    nodeName,
			UserPodNs:   userPod.Namespace,
			UserPodName: userPod.Name,
		})
	}
	return targets, nil
}

// findCsiNodePodOnNode locates the driver's own csi-node DaemonSet pod
// running on the given node, along with the container that has fsfreeze
// available (the hs-csi-plugin-node container).
func (f *Freezer) findCsiNodePodOnNode(ctx context.Context, nodeName string) (*corev1.Pod, string, error) {
	// The csi-node DS is deployed in kube-system with label app=csi-node
	// (matching the bundled plugin.yaml). Filter by that + spec.nodeName.
	list, err := f.clientset.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{
		LabelSelector: "app=csi-node",
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, "", err
	}
	for i := range list.Items {
		p := &list.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		return p, "hs-csi-plugin-node", nil
	}
	return nil, "", fmt.Errorf("no running csi-node pod on %s", nodeName)
}

// execFsfreeze runs `fsfreeze <op> <mountPath>` inside the target pod/container
// via the k8s API's exec subresource.
func (f *Freezer) execFsfreeze(ctx context.Context, t FrozenTarget, op string) error {
	cmd := []string{"fsfreeze", op, t.MountPath}
	req := f.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(t.Namespace).
		Name(t.PodName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: t.Container,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exe, err := remotecommand.NewSPDYExecutor(f.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build SPDY executor: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exe.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("stream: %w (stderr=%q)", err, stderr.String())
	}
	return nil
}
