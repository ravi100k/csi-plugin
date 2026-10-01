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

import "testing"

func TestFileChildrenEntryName(t *testing.T) {
	for _, tc := range []struct {
		child FileChildren
		want  string
	}{
		{FileChildren{Name: "snap-1", Path: "/s/.snapshot/snap-1"}, "snap-1"},
		// Anvil 5.3.1: empty name, path with a trailing slash.
		{FileChildren{Path: "/s/.snapshot/2026-10-01T15-25-39-0/"}, "2026-10-01T15-25-39-0"},
		{FileChildren{Path: "/s/.snapshot/current"}, "current"},
	} {
		if got := tc.child.EntryName(); got != tc.want {
			t.Errorf("EntryName(%+v) = %q, want %q", tc.child, got, tc.want)
		}
	}
}

func TestShareSnapshotCreateTime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		createTime int64
		want       int64
	}{
		{"2026-10-01T15-25-39-0", 1700000000, 1700000000}, // reported time wins
		{"2026-10-01T15-25-39-0", 0, 1790868339},          // 2026-10-01T15:25:39Z
		{"2026-10-01T15-25-39-12", 0, 1790868339},
		{"snap-1", 0, 0},
		{"", 0, 0},
	} {
		if got := ShareSnapshotCreateTime(tc.name, tc.createTime); got != tc.want {
			t.Errorf("ShareSnapshotCreateTime(%q, %d) = %d, want %d", tc.name, tc.createTime, got, tc.want)
		}
	}
}
