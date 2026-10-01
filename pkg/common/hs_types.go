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
	"encoding/json"
	"path"
	"strings"
	"time"
)

// Structures to hold information about a plugin created volume
type HSVolumeParameters struct {
	DeleteDelay            int64
	ExportOptions          []ShareExportOptions
	Objectives             []string
	BlockBackingShareName  string
	MountBackingShareName  string
	VolumeNameFormat       string
	FSType                 string
	Comment                string
	AdditionalMetadataTags map[string]string
	CacheEnabled           bool
	FQDN                   string
	MountFlags             []string
	ObjectiveTarget        string
}

type HSVolume struct {
	DeleteDelay            int64
	ExportOptions          []ShareExportOptions
	Objectives             []string
	BlockBackingShareName  string
	MountBackingShareName  string
	Size                   int64
	Name                   string
	Path                   string
	VolumeMode             string
	SourceSnapPath         string
	FSType                 string
	Comment                string
	SourceSnapShareName    string
	AdditionalMetadataTags map[string]string
	FQDN                   string
	MountFlags             []string
	ObjectiveTarget        string
}

///// Request and Response objects for interacting with the HS API

// We must create separate req and response objects since the API does not allow
// specifying unused fields
type ClusterResponse struct {
	Capacity map[string]int64 `json:"capacity"`
}

type ShareRequest struct {
	Name            string                  `json:"name"`
	ExportPath      string                  `json:"path"`
	Comment         string                  `json:"comment"`
	ExtendedInfo    map[string]string       `json:"extendedInfo"`
	Size            int64                   `json:"shareSizeLimit,omitempty"`
	ExportOptions   []ShareExportOptions    `json:"exportOptions,omitempty"`
	ShareObjectives []ShareObjectiveRequest `json:"shareObjectives,omitempty"`
}

type ShareUpdateRequest struct {
	Name         string            `json:"name"`
	Comment      string            `json:"comment"`
	ExtendedInfo map[string]string `json:"extendedInfo"`
}

type ShareResponse struct {
	Name          string               `json:"name"`
	ExportPath    string               `json:"path"`
	Comment       string               `json:"comment"`
	ExtendedInfo  map[string]string    `json:"extendedInfo"`
	ShareState    string               `json:"shareState"`
	Size          int64                `json:"shareSizeLimit"`
	ExportOptions []ShareExportOptions `json:"exportOptions"`
	Space         ShareSpaceResponse   `json:"space"`
	Inodes        ShareInodesResponse  `json:"inodes"`
	Objectives    ObjectivesResponse   `json:"objectives"`
}

type ShareSpaceResponse struct {
	Used      int64 `json:"used"`
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	Percent   int64 `json:"percent"`
}

type ShareInodesResponse struct {
	Used      int64 `json:"used"`
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	Percent   int64 `json:"percent"`
}

type ShareExportOptions struct {
	Subnet            string `json:"subnet"`
	AccessPermissions string `json:"accessPermissions"` // Must be "RO" or "RW"
	RootSquash        bool   `json:"rootSquash"`
}
type ObjectivesResponse struct {
	Applied []AppliedObjectiveResponse `json:"appliedObjectives"`
}
type AppliedObjectiveResponse struct {
	Name string `json:"name"`
}
type ClusterObjectiveResponse struct {
	Name   string                 `json:"name"`
	Fields map[string]interface{} `json:"-"`
}

func (o *ClusterObjectiveResponse) UnmarshalJSON(data []byte) error {
	var fields map[string]interface{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	o.Fields = fields
	if name, ok := fields["name"].(string); ok {
		o.Name = name
	}

	return nil
}

func (o ClusterObjectiveResponse) MarshalJSON() ([]byte, error) {
	if o.Fields != nil {
		return json.Marshal(o.Fields)
	}

	type objectiveAlias ClusterObjectiveResponse
	return json.Marshal(objectiveAlias(o))
}

type ShareObjectiveRequest struct {
	Objective ClusterObjectiveResponse `json:"objective"`
}

type Task struct {
	Uuid          string        `json:"uuid"`
	Action        string        `json:"name"`
	Status        string        `json:"status"`
	Progress      ProgressValue `json:"progress"` // normalized to 0.0–1.0
	StatusMessage string        `json:"statusMessage"`
	ParamsMap     TaskParamsMap `json:"paramsMap"`
}

type TaskParamsMap struct {
	CreatePath      string `json:"create-path"`
	CreatedBy       string `json:"created-by"`
	CreatedByName   string `json:"created-by-name"`
	Name            string `json:"name"`
	OverideMemCheck string `json:"override-mem-check"`
}

type File struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	Size     int64          `json:"size"`
	Children []FileChildren `json:"children"`
}

type FileChildren struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Parent     string `json:"parent"`
	SharePath  string `json:"sharePath"`
	ShareName  string `json:"shareName"`
	CreateTime int64  `json:"createTime"`
}

// EntryName returns the child's name. Some Anvil versions (seen on 5.3.1)
// leave "name" empty for the entries of a directory listed without a trailing
// slash, so fall back to the last element of its path ("<dir>/<name>/").
func (c FileChildren) EntryName() string {
	if c.Name != "" {
		return c.Name
	}
	return path.Base(strings.TrimSuffix(c.Path, "/"))
}

// shareSnapshotNameLayout is the timestamp prefix of an Anvil share snapshot
// name, e.g. "2026-10-01T15-25-39" in "2026-10-01T15-25-39-0".
const shareSnapshotNameLayout = "2006-01-02T15-04-05"

// ShareSnapshotCreateTime returns a share snapshot's creation time in Unix
// seconds. The Anvil may report createTime as null for .snapshot entries, so
// fall back to the timestamp the snapshot is named after (taken as UTC), and
// to 0 when neither is available.
func ShareSnapshotCreateTime(name string, createTime int64) int64 {
	if createTime > 0 {
		return createTime
	}
	if len(name) >= len(shareSnapshotNameLayout) {
		if t, err := time.Parse(shareSnapshotNameLayout, name[:len(shareSnapshotNameLayout)]); err == nil {
			return t.Unix()
		}
	}
	return 0
}

type FileSnapshot struct {
	SourceFilename string `json:"sourceFilename"`
	Time           string `json:"time"`
}

type Cluster struct {
	Name              string              `json:"name"`
	PortalFloatingIps []PortalFloatingIps `json:"portalFloatingIps"`
}

// Portal Data Floating IPs are a cluster-wide resource
type PortalFloatingIps struct {
	Address      string `json:"address"`
	PrefixLength int    `json:"prefixLength"`
}

type DataPortal struct {
	OperState      string            `json:"operState"`      // We want 'UP'
	AdminState     string            `json:"adminState"`     // We want 'UP'
	DataPortalType string            `json:"dataPortalType"` // We want NFS_V3
	Exported       []string          `json:"exported"`
	Node           DataPortalNode    `json:"node"`
	Uoid           map[string]string `json:"uoid"`
}

type DataPortalNodeAddress struct {
	Address      string `json:"address"`
	PrefixLength int    `json:"prefixLength"`
}

type DataPortalNode struct {
	Name          string                `json:"name"`
	MgmtIpAddress DataPortalNodeAddress `json:"mgmtIpAddress"` // do we want this or some data ip?
}

type VolumeResponse struct {
	Name               string `json:"name"`
	Created            int64  `json:"created"`
	Modified           int64  `json:"modified"`
	OperatingState     string `json:"operState"`
	StorageVolumeState string `json:"storageVolumeState"`
	Capacity           int64  `json:"effectiveTotalCapacity"`
}

type SnapshotResponse struct {
	Id             string `json:"name"`
	Size           int64  `json:"size"`
	Created        int64  `json:"created"`
	ReadyToUse     bool   `json:"ReadyToUse"`
	SourceVolumeId string `json:"ShareName"`
}
