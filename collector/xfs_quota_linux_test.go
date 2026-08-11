// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !noxfsquota

package collector

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/sys/unix"
)

type testXFSQuotaCollector struct {
	collector *xfsQuotaCollector
}

func (c testXFSQuotaCollector) Collect(ch chan<- prometheus.Metric) {
	_ = c.collector.Update(ch)
}

func (c testXFSQuotaCollector) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(c, ch)
}

func TestXFSDiskQuotaLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(xfsDiskQuota{}), uintptr(112); got != want {
		t.Fatalf("unexpected fs_disk_quota size: got %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(xfsDiskQuota{}.BlockHardLimit), uintptr(8); got != want {
		t.Fatalf("unexpected block hard limit offset: got %d, want %d", got, want)
	}
	if got, want := unsafe.Offsetof(xfsDiskQuota{}.RealtimeBlockHardLimit), uintptr(72); got != want {
		t.Fatalf("unexpected realtime block hard limit offset: got %d, want %d", got, want)
	}
}

func TestXFSGetNextProjectQuotaCommand(t *testing.T) {
	if got, want := qXGetNextProjectQuota, uintptr(0x580902); got != want {
		t.Fatalf("unexpected Q_XGETNEXTQUOTA command: got %#x, want %#x", got, want)
	}
}

func TestXFSQuotaCollector(t *testing.T) {
	collector := newTestXFSQuotaCollector()

	expected := `# HELP node_xfs_quota_hard_limit_bytes Hard limit in bytes for an XFS project quota.
# TYPE node_xfs_quota_hard_limit_bytes gauge
node_xfs_quota_hard_limit_bytes{device="/dev/sda1",project_id="42"} 4096
# HELP node_xfs_quota_hard_limit_inodes Hard inode limit for an XFS project quota.
# TYPE node_xfs_quota_hard_limit_inodes gauge
node_xfs_quota_hard_limit_inodes{device="/dev/sda1",project_id="42"} 7
# HELP node_xfs_quota_soft_limit_bytes Soft limit in bytes for an XFS project quota.
# TYPE node_xfs_quota_soft_limit_bytes gauge
node_xfs_quota_soft_limit_bytes{device="/dev/sda1",project_id="42"} 2048
# HELP node_xfs_quota_soft_limit_inodes Soft inode limit for an XFS project quota.
# TYPE node_xfs_quota_soft_limit_inodes gauge
node_xfs_quota_soft_limit_inodes{device="/dev/sda1",project_id="42"} 5
# HELP node_xfs_quota_used_bytes Number of bytes used by an XFS project quota.
# TYPE node_xfs_quota_used_bytes gauge
node_xfs_quota_used_bytes{device="/dev/sda1",project_id="42"} 1024
# HELP node_xfs_quota_used_inodes Number of inodes used by an XFS project quota.
# TYPE node_xfs_quota_used_inodes gauge
node_xfs_quota_used_inodes{device="/dev/sda1",project_id="42"} 3
`

	if err := testutil.CollectAndCompare(testXFSQuotaCollector{collector}, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestXFSQuotaCollectorProjectInfo(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	collector.projectInfoEnabled = true
	collector.mountPointDetails = func(*slog.Logger) ([]filesystemLabels, error) {
		return []filesystemLabels{
			{device: "/dev/sda1", mountPoint: "/xfs", fsType: "xfs"},
			{device: "/dev/sdb1", mountPoint: "/xfs/nested", fsType: "xfs"},
		}, nil
	}
	collector.getNextProjectQuota = func(string, uint32) (xfsDiskQuota, error) {
		return xfsDiskQuota{}, unix.ESRCH
	}
	collector.readProjectPaths = func(path string) ([]xfsProjectPath, error) {
		if path != rootfsFilePath("/etc/projects") {
			t.Fatalf("unexpected projects file: %q", path)
		}
		return []xfsProjectPath{
			{id: 42, path: "/xfs/team-a"},
			{id: 7, path: "/xfs/nested/team-b"},
			{id: 7, path: "/xfs/nested/team-b"},
			{id: 100, path: "/not-mounted"},
		}, nil
	}

	expected := `# HELP node_xfs_quota_project_info Information about an XFS project path from /etc/projects.
# TYPE node_xfs_quota_project_info gauge
node_xfs_quota_project_info{device="/dev/sda1",path="/xfs/team-a",project_id="42"} 1
node_xfs_quota_project_info{device="/dev/sdb1",path="/xfs/nested/team-b",project_id="7"} 1
`

	if err := testutil.CollectAndCompare(
		testXFSQuotaCollector{collector},
		strings.NewReader(expected),
		"node_xfs_quota_project_info",
	); err != nil {
		t.Fatal(err)
	}
}

func TestParseXFSProjectPaths(t *testing.T) {
	projects, err := parseXFSProjectPaths(strings.NewReader(`
# project paths
42:/xfs/team-a
7: /xfs/team-b
`))
	if err != nil {
		t.Fatal(err)
	}

	want := []xfsProjectPath{
		{id: 42, path: "/xfs/team-a"},
		{id: 7, path: "/xfs/team-b"},
	}
	if len(projects) != len(want) {
		t.Fatalf("unexpected number of project paths: got %d, want %d", len(projects), len(want))
	}
	for i := range want {
		if projects[i] != want[i] {
			t.Errorf("unexpected project path %d: got %+v, want %+v", i, projects[i], want[i])
		}
	}
}

func TestParseXFSProjectPathsRejectsInvalidEntry(t *testing.T) {
	if _, err := parseXFSProjectPaths(strings.NewReader("42:relative/path\n")); err == nil {
		t.Fatal("expected an error for a relative project path")
	}
}

func TestXFSQuotaCollectorEnumeratesEachDeviceOnce(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	calls := []uint32{}
	collector.getNextProjectQuota = func(device string, projectID uint32) (xfsDiskQuota, error) {
		if device != rootfsFilePath("/dev/sda1") {
			t.Fatalf("unexpected device: %q", device)
		}
		calls = append(calls, projectID)
		switch projectID {
		case 0:
			return xfsDiskQuota{ID: 42}, nil
		case 43:
			return xfsDiskQuota{ID: 100}, nil
		default:
			return xfsDiskQuota{}, unix.ESRCH
		}
	}

	ch := make(chan prometheus.Metric, 12)
	if err := collector.Update(ch); err != nil {
		t.Fatal(err)
	}
	if got, want := len(calls), 3; got != want {
		t.Fatalf("unexpected number of quotactl calls: got %d, want %d", got, want)
	}
	for i, want := range []uint32{0, 43, 101} {
		if calls[i] != want {
			t.Errorf("unexpected project ID for call %d: got %d, want %d", i, calls[i], want)
		}
	}
}

func TestXFSQuotaCollectorNoData(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	collector.mountPointDetails = func(*slog.Logger) ([]filesystemLabels, error) {
		return []filesystemLabels{{device: "/dev/sda1", fsType: "ext4"}}, nil
	}

	if err := collector.Update(make(chan prometheus.Metric)); !IsNoDataError(err) {
		t.Fatalf("unexpected error: got %v, want ErrNoData", err)
	}
}

func TestXFSQuotaCollectorError(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	expected := errors.New("quotactl failed")
	collector.getNextProjectQuota = func(string, uint32) (xfsDiskQuota, error) {
		return xfsDiskQuota{}, expected
	}

	if err := collector.Update(make(chan prometheus.Metric)); !errors.Is(err, expected) {
		t.Fatalf("unexpected error: got %v, want %v", err, expected)
	}
}

func newTestXFSQuotaCollector() *xfsQuotaCollector {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	createdCollector, err := NewXFSQuotaCollector(logger)
	if err != nil {
		panic(err)
	}
	collector := createdCollector.(*xfsQuotaCollector)
	collector.projectInfoEnabled = false
	collector.mountPointDetails = func(*slog.Logger) ([]filesystemLabels, error) {
		return []filesystemLabels{
			{device: "/dev/sda1", mountPoint: "/xfs", fsType: "xfs"},
			{device: "/dev/sda1", mountPoint: "/xfs-bind", fsType: "xfs"},
			{device: "/dev/sdb1", mountPoint: "/ext4", fsType: "ext4"},
		}, nil
	}
	collector.getNextProjectQuota = func(device string, projectID uint32) (xfsDiskQuota, error) {
			if device != rootfsFilePath("/dev/sda1") {
				return xfsDiskQuota{}, fmt.Errorf("unexpected device: %q", device)
			}
			if projectID != 0 {
				return xfsDiskQuota{}, unix.ESRCH
			}
			return xfsDiskQuota{
				ID:             42,
				BlockCount:     2,
				BlockSoftLimit: 4,
				BlockHardLimit: 8,
				InodeCount:     3,
				InodeSoftLimit: 5,
				InodeHardLimit: 7,
			}, nil
	}

	return collector
}
