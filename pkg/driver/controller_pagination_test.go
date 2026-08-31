package driver

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPaginationStart(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		total   int
		want    int
		wantErr codes.Code
	}{
		{name: "first page", total: 3, want: 0},
		{name: "next page", token: "2", total: 3, want: 2},
		{name: "end", token: "3", total: 3, want: 3},
		{name: "malformed", token: "nope", total: 3, wantErr: codes.Aborted},
		{name: "negative", token: "-1", total: 3, wantErr: codes.Aborted},
		{name: "past end", token: "4", total: 3, wantErr: codes.Aborted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := paginationStart(tt.token, tt.total)
			if status.Code(err) != tt.wantErr {
				t.Fatalf("error code = %s, want %s (err=%v)", status.Code(err), tt.wantErr, err)
			}
			if got != tt.want {
				t.Fatalf("start = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBackendSnapshotNameIsDeterministicAndBounded(t *testing.T) {
	a := backendSnapshotName("snapshot-123")
	b := backendSnapshotName("snapshot-123")
	if a != b {
		t.Fatalf("same CSI name produced different backend names: %q != %q", a, b)
	}
	if !strings.HasPrefix(a, "csi-") || len(a) != 68 {
		t.Fatalf("unexpected deterministic snapshot name %q", a)
	}
	if a == backendSnapshotName("snapshot-456") {
		t.Fatal("different CSI names produced the same backend name")
	}
}
