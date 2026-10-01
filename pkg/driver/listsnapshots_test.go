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

package driver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/hammer-space/csi-plugin/pkg/client"
)

// newListSnapshotsDriver wires a CSIDriver to a fake Anvil exposing one
// share-backed volume ("/pvc-share" with snapshot "snap-1"), one file-backed
// volume ("/backing/pvc-file" with a snapshot under .fsnapshot), and one share
// ("/pvc-broken") whose .snapshot directory cannot be read.
func newListSnapshotsDriver(t *testing.T) (*CSIDriver, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(client.BasePath+"/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc(client.BasePath+"/shares/pvc-share", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"pvc-share","path":"/pvc-share"}`)
	})
	mux.HandleFunc(client.BasePath+"/shares/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	// ListShares, used by the enumeration path (no snapshot_id supplied).
	mux.HandleFunc(client.BasePath+"/shares", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"name":"pvc-share","path":"/pvc-share"},{"name":"pvc-broken","path":"/pvc-broken"}]`)
	})
	// pvc-ts answers the way Anvil 5.3.1 does: listing .snapshot without a
	// trailing slash gives entries with an empty name and a null size and
	// createTime, identified only by their path.
	mux.HandleFunc(client.BasePath+"/shares/pvc-ts", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"pvc-ts","path":"/pvc-ts"}`)
	})
	mux.HandleFunc(client.BasePath+"/share-snapshots/snapshot-list/pvc-ts", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `["2026-10-01T15-25-39-0","current"]`)
	})
	mux.HandleFunc(client.BasePath+"/share-snapshots/snapshot-list/pvc-share", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `["current","snap-1"]`)
	})
	mux.HandleFunc(client.BasePath+"/files", func(w http.ResponseWriter, r *http.Request) {
		// Like the Anvil, a snapshot directory reports its aggregate size only
		// when asked with getParentDirSize=true; otherwise its size is null.
		dirSize := func(size int64) {
			if r.URL.Query().Get("getParentDirSize") == "true" {
				fmt.Fprintf(w, `{"name":"","size":%d,"children":[]}`, size)
				return
			}
			fmt.Fprint(w, `{"name":"","size":null,"children":[]}`)
		}
		switch r.URL.Query().Get("path") {
		case "/pvc-share/.snapshot/snap-1":
			dirSize(1073741824)
		case "/pvc-ts/.snapshot/2026-10-01T15-25-39-0":
			dirSize(20971520)
		// findSnapshotByID asks without a trailing slash; the enumeration path
		// (hsclient.ListSnapshots) asks with one.
		case "/pvc-share/.snapshot", "/pvc-share/.snapshot/":
			fmt.Fprint(w, `{"name":".snapshot","children":[{"name":"current","size":0,"createTime":0},{"name":"snap-1","size":1073741824,"createTime":1700000000}]}`)
		case "/pvc-ts/.snapshot":
			fmt.Fprint(w, `{"name":".snapshot","children":[{"name":"","path":"/pvc-ts/.snapshot/current/","size":null,"createTime":null},{"name":"","path":"/pvc-ts/.snapshot/2026-10-01T15-25-39-0/","size":null,"createTime":null}]}`)
		case "/pvc-broken/.snapshot/":
			w.WriteHeader(403)
		case "/backing/.fsnapshot/2026-09-22-00-00-00":
			fmt.Fprint(w, `{"name":"2026-09-22-00-00-00","children":[{"name":"pvc-file","size":2147483648,"createTime":1700000001}]}`)
		default:
			w.WriteHeader(404)
		}
	})
	server := httptest.NewServer(mux)
	hs, err := client.NewHammerspaceClient(server.URL, "test", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	return &CSIDriver{hsclient: hs}, server.Close
}

// The external snapshotter marks a PRE-PROVISIONED VolumeSnapshotContent ready
// only when ListSnapshots returns its snapshot handle. The handle is the
// composite ID CreateSnapshot issued, so looking it up must not fail just
// because the backend knows the snapshot by its bare name.
func TestListSnapshotsResolvesPreProvisionedHandles(t *testing.T) {
	driver, closeServer := newListSnapshotsDriver(t)
	defer closeServer()

	for _, tc := range []struct {
		name          string
		snapshotID    string
		wantFound     bool
		wantSize      int64
		wantSourceVol string
	}{
		{
			name:          "share-backed",
			snapshotID:    "snap-1|/pvc-share",
			wantFound:     true,
			wantSize:      1073741824,
			wantSourceVol: "/pvc-share",
		},
		{
			name:          "file-backed",
			snapshotID:    "/backing/.fsnapshot/2026-09-22-00-00-00/pvc-file|/backing/pvc-file",
			wantFound:     true,
			wantSize:      2147483648,
			wantSourceVol: "/backing/pvc-file",
		},
		{name: "unknown share snapshot", snapshotID: "no-such-snap|/pvc-share"},
		{name: "unknown source share", snapshotID: "snap-1|/no-such-share"},
		{name: "unknown file snapshot", snapshotID: "/backing/.fsnapshot/1999-01-01-00-00-00/pvc-file|/backing/pvc-file"},
		{name: "malformed handle", snapshotID: "not-a-composite-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := driver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: tc.snapshotID})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantFound {
				if len(resp.Entries) != 0 {
					t.Fatalf("expected no entries for %q, got %v", tc.snapshotID, resp.Entries)
				}
				return
			}
			if len(resp.Entries) != 1 {
				t.Fatalf("expected exactly one entry for %q, got %v", tc.snapshotID, resp.Entries)
			}
			snapshot := resp.Entries[0].Snapshot
			if snapshot.SnapshotId != tc.snapshotID {
				t.Fatalf("snapshot ID = %q, want the handle we asked for, %q", snapshot.SnapshotId, tc.snapshotID)
			}
			if !snapshot.ReadyToUse {
				t.Fatal("an existing snapshot must be reported ready, or the content never binds")
			}
			if snapshot.SourceVolumeId != tc.wantSourceVol {
				t.Fatalf("source volume = %q, want %q", snapshot.SourceVolumeId, tc.wantSourceVol)
			}
			if snapshot.SizeBytes != tc.wantSize {
				t.Fatalf("size = %d, want %d", snapshot.SizeBytes, tc.wantSize)
			}
		})
	}
}

// Filtering by both snapshot_id and source_volume_id must agree: a handle
// belonging to a different volume is not a match.
func TestListSnapshotsFiltersHandleBySourceVolume(t *testing.T) {
	driver, closeServer := newListSnapshotsDriver(t)
	defer closeServer()

	resp, err := driver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SnapshotId:     "snap-1|/pvc-share",
		SourceVolumeId: "/some-other-volume",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("expected no entries, got %v", resp.Entries)
	}
}

// The CO identifies a share-backed volume by its export path ("/pvc-share"),
// while the backend knows it as a bare share name ("pvc-share"). Filtering the
// enumeration on the name while reporting the path made every
// source_volume_id-filtered ListSnapshots return nothing.
func TestListSnapshotsFiltersEnumerationByExportPath(t *testing.T) {
	driver, closeServer := newListSnapshotsDriver(t)
	defer closeServer()

	resp, err := driver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "/pvc-share",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected the share's snapshot to be listed for its export path, got %d entries", len(resp.Entries))
	}
	got := resp.Entries[0].Snapshot
	if got.SourceVolumeId != "/pvc-share" {
		t.Fatalf("source volume = %q, want %q", got.SourceVolumeId, "/pvc-share")
	}
	if got.SnapshotId != "snap-1|/pvc-share" {
		t.Fatalf("snapshot ID = %q, want the composite handle %q", got.SnapshotId, "snap-1|/pvc-share")
	}

	// A volume that is not this share must not match.
	resp, err = driver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "/some-other-volume",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Entries) != 0 {
		t.Fatalf("expected no entries for an unrelated volume, got %d", len(resp.Entries))
	}
}

// Listing with no filter must report every share's snapshots under the same
// composite ID CreateSnapshot issued, leave out the share's live "current"
// view, and not let one unreadable share fail the whole listing.
func TestListSnapshotsEnumeratesAllShares(t *testing.T) {
	driver, closeServer := newListSnapshotsDriver(t)
	defer closeServer()

	resp, err := driver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	if err != nil {
		t.Fatalf("an unreadable share must not fail the listing: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected only snap-1 (not \"current\"), got %v", resp.Entries)
	}
	got := resp.Entries[0].Snapshot
	if got.SnapshotId != "snap-1|/pvc-share" {
		t.Fatalf("snapshot ID = %q, want the composite handle %q", got.SnapshotId, "snap-1|/pvc-share")
	}
	if got.SourceVolumeId != "/pvc-share" {
		t.Fatalf("source volume = %q, want %q", got.SourceVolumeId, "/pvc-share")
	}
}

// Anvil 5.3.1 reports createTime as null for .snapshot entries. The creation
// time must then come from the snapshot's timestamped name, not be reported as
// the Unix epoch.
func TestListSnapshotsSizeAndCreationTimeOnAnvil531(t *testing.T) {
	driver, closeServer := newListSnapshotsDriver(t)
	defer closeServer()

	resp, err := driver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SnapshotId: "2026-10-01T15-25-39-0|/pvc-ts",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("expected exactly one entry, got %v", resp.Entries)
	}
	const want = 1790868339 // 2026-10-01T15:25:39Z
	if got := resp.Entries[0].Snapshot.CreationTime.GetSeconds(); got != want {
		t.Fatalf("creation time = %d, want %d", got, want)
	}
	// The .snapshot listing reports a null size; the snapshot directory's
	// aggregate size must be asked for directly.
	if got := resp.Entries[0].Snapshot.SizeBytes; got != 20971520 {
		t.Fatalf("size = %d, want %d", got, 20971520)
	}
}
