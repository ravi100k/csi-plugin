package driver

import (
	"context"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLogGRPCDoesNotLogRequestSecrets(t *testing.T) {
	hook := logtest.NewGlobal()
	defer hook.Reset()
	previousLevel := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	defer log.SetLevel(previousLevel)

	request := &csi.NodePublishVolumeRequest{
		VolumeId: "volume-a",
		Secrets:  map[string]string{"password": "do-not-log-this"},
	}
	logGRPC("/csi.v1.Node/NodePublishVolume", request, &csi.NodePublishVolumeResponse{}, nil)

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("expected a debug log entry")
	}
	if _, exists := entry.Data["request"]; exists {
		t.Fatal("gRPC log contains serialized request field")
	}
	if got := entry.Data["request_type"]; got != "*csi.NodePublishVolumeRequest" {
		t.Fatalf("request_type = %v", got)
	}
}

func TestControllerLeaderElectionGate(t *testing.T) {
	d := &CSIDriver{}
	d.EnableControllerLeaderElection()
	called := false
	handler := func(context.Context, interface{}) (interface{}, error) {
		called = true
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Controller/CreateVolume"}
	if _, err := d.callInterceptor(context.Background(), nil, info, handler); status.Code(err) != codes.Unavailable {
		t.Fatalf("non-leader status = %v, want Unavailable", status.Code(err))
	}
	if called {
		t.Fatal("controller handler ran on a non-leader")
	}

	d.SetControllerLeader(true)
	if _, err := d.callInterceptor(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("leader request failed: %v", err)
	}
	if !called {
		t.Fatal("controller handler did not run on leader")
	}
}

// TestAcquireAndReleaseVolumeLock ensures a lock can be acquired and released.
func TestAcquireAndReleaseVolumeLock(t *testing.T) {
	d := &CSIDriver{
		volumeLocks:   make(map[string]*keyLock),
		snapshotLocks: make(map[string]*keyLock),
	}

	ctx := context.Background()
	volID := "vol-test"

	unlock, err := d.acquireVolumeLock(ctx, volID)
	if err != nil {
		t.Fatalf("expected lock to succeed, got error: %v", err)
	}
	if unlock == nil {
		t.Fatalf("expected non-nil unlock function")
	}

	unlock()
	_, err = d.acquireVolumeLock(ctx, volID)
	if err != nil {
		t.Fatalf("expected lock to succeed after unlock, got error: %v", err)
	}
}

// TestAcquireVolumeLockTimeout ensures lock acquisition times out correctly.
func TestAcquireVolumeLockTimeout(t *testing.T) {
	d := &CSIDriver{
		volumeLocks:   make(map[string]*keyLock),
		snapshotLocks: make(map[string]*keyLock),
	}

	volID := "vol-timeout"

	// Acquire the lock and don't release
	unlock, err := d.acquireVolumeLock(context.Background(), volID)
	if err != nil {
		t.Fatalf("expected first acquire to succeed, got error: %v", err)
	}
	if unlock == nil {
		t.Fatalf("expected unlock function to be non-nil")
	}

	// Try acquiring again with short timeout
	start := time.Now()
	_, err = d.acquireVolumeLock(context.Background(), volID)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error but got none")
	}
	// PR A: a lock-acquire timeout must return codes.Aborted (retryable) rather
	// than calling os.Exit(1), which previously crashed the whole controller
	// under concurrent load. If this regresses to os.Exit, the test binary dies
	// here and the failure is unmistakable.
	if status.Code(err) != codes.Aborted {
		t.Fatalf("expected codes.Aborted on lock timeout, got %v (err: %v)", status.Code(err), err)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("expected blocking for ~300ms, got only %v", elapsed)
	}
}

// TestSnapshotLock is just to ensure snapshotLocks uses same logic
func TestAcquireSnapshotLock(t *testing.T) {
	d := &CSIDriver{
		volumeLocks:   make(map[string]*keyLock),
		snapshotLocks: make(map[string]*keyLock),
	}

	snapID := "snap-1"
	unlock, err := d.acquireSnapshotLock(context.Background(), snapID)
	if err != nil {
		t.Fatalf("expected snapshot lock to succeed, got error: %v", err)
	}
	if unlock == nil {
		t.Fatalf("expected non-nil unlock function")
	}

	// Release and ensure we can lock again
	unlock()
	_, err = d.acquireSnapshotLock(context.Background(), snapID)
	if err != nil {
		t.Fatalf("expected lock after unlock to succeed, got error: %v", err)
	}
}
