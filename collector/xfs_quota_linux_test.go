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
	"io"
	"log/slog"
	"strings"
	"testing"

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

func TestXFSQuotaCollector(t *testing.T) {
	collector := newTestXFSQuotaCollector()

	expected := `# HELP node_xfs_quota_avail_bytes Effective space available to non-root users in bytes reported by statfs for an XFS project quota path.
# TYPE node_xfs_quota_avail_bytes gauge
node_xfs_quota_avail_bytes{device="/dev/sda1",device_error="",path="/xfs/team-a"} 1536
# HELP node_xfs_quota_device_error Whether an error occurred while getting statistics for the given XFS project quota path.
# TYPE node_xfs_quota_device_error gauge
node_xfs_quota_device_error{device="/dev/sda1",device_error="",path="/xfs/team-a"} 0
# HELP node_xfs_quota_files Effective total file nodes reported by statfs for an XFS project quota path.
# TYPE node_xfs_quota_files gauge
node_xfs_quota_files{device="/dev/sda1",device_error="",path="/xfs/team-a"} 100
# HELP node_xfs_quota_files_free Effective free file nodes reported by statfs for an XFS project quota path.
# TYPE node_xfs_quota_files_free gauge
node_xfs_quota_files_free{device="/dev/sda1",device_error="",path="/xfs/team-a"} 75
# HELP node_xfs_quota_free_bytes Effective free space in bytes reported by statfs for an XFS project quota path.
# TYPE node_xfs_quota_free_bytes gauge
node_xfs_quota_free_bytes{device="/dev/sda1",device_error="",path="/xfs/team-a"} 2048
# HELP node_xfs_quota_size_bytes Effective size in bytes reported by statfs for an XFS project quota path.
# TYPE node_xfs_quota_size_bytes gauge
node_xfs_quota_size_bytes{device="/dev/sda1",device_error="",path="/xfs/team-a"} 10240
`

	if err := testutil.CollectAndCompare(testXFSQuotaCollector{collector}, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

func TestXFSQuotaCollectorUsesRootfsAndDeduplicatesQuota(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	collector.projectsFile = "/host/etc/projects"
	collector.readProjectPaths = func(path string) ([]xfsProjectPath, error) {
		if path != "/host/etc/projects" {
			t.Fatalf("unexpected projects file: %q", path)
		}
		return []xfsProjectPath{
			{id: 42, path: "/xfs/team-a"},
			{id: 43, path: "/xfs/team-a"},
		}, nil
	}

	calls := make([]string, 0)
	collector.statfs = func(path string, stats *unix.Statfs_t) error {
		calls = append(calls, path)
		*stats = unix.Statfs_t{Bsize: 4096}
		return nil
	}

	if err := collector.Update(make(chan prometheus.Metric, 6)); err != nil {
		t.Fatal(err)
	}
	if got, want := len(calls), 1; got != want {
		t.Fatalf("unexpected number of statfs calls: got %d, want %d", got, want)
	}
	if got, want := calls[0], rootfsFilePath("/xfs/team-a"); got != want {
		t.Fatalf("unexpected statfs path: got %q, want %q", got, want)
	}
}

func TestXFSQuotaCollectorReloadsProjectsFile(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	reads := 0
	collector.readProjectPaths = func(path string) ([]xfsProjectPath, error) {
		if path != "/etc/projects" {
			t.Fatalf("unexpected projects file: %q", path)
		}
		reads++
		if reads == 1 {
			return []xfsProjectPath{{id: 42, path: "/xfs/team-a"}}, nil
		}
		return []xfsProjectPath{{id: 7, path: "/xfs/nested/team-b"}}, nil
	}

	paths := make([]string, 0, 2)
	collector.statfs = func(path string, stats *unix.Statfs_t) error {
		paths = append(paths, path)
		*stats = unix.Statfs_t{Bsize: 4096}
		return nil
	}

	if err := collector.Update(make(chan prometheus.Metric, 6)); err != nil {
		t.Fatal(err)
	}
	if err := collector.Update(make(chan prometheus.Metric, 6)); err != nil {
		t.Fatal(err)
	}
	if got, want := reads, 2; got != want {
		t.Fatalf("unexpected number of projects file reads: got %d, want %d", got, want)
	}
	wantPaths := []string{rootfsFilePath("/xfs/team-a"), rootfsFilePath("/xfs/nested/team-b")}
	if got, want := len(paths), len(wantPaths); got != want {
		t.Fatalf("unexpected number of statfs calls: got %d, want %d", got, want)
	}
	for i := range wantPaths {
		if paths[i] != wantPaths[i] {
			t.Errorf("unexpected statfs path %d: got %q, want %q", i, paths[i], wantPaths[i])
		}
	}
}

func TestXFSQuotaCollectorReadProjectsError(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	expected := errors.New("read failed")
	collector.readProjectPaths = func(string) ([]xfsProjectPath, error) {
		return nil, expected
	}

	if err := collector.Update(make(chan prometheus.Metric)); !errors.Is(err, expected) {
		t.Fatalf("unexpected error: got %v, want %v", err, expected)
	}
}

func TestXFSQuotaCollectorStatfsError(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	expected := errors.New("statfs failed")
	collector.readProjectPaths = func(string) ([]xfsProjectPath, error) {
		return []xfsProjectPath{
			{id: 42, path: "/xfs/team-a"},
			{id: 7, path: "/xfs/nested/team-b"},
		}, nil
	}
	collector.statfs = func(path string, stats *unix.Statfs_t) error {
		if path == rootfsFilePath("/xfs/team-a") {
			return expected
		}
		*stats = unix.Statfs_t{Bsize: 4096}
		return nil
	}

	expectedMetrics := `# HELP node_xfs_quota_device_error Whether an error occurred while getting statistics for the given XFS project quota path.
# TYPE node_xfs_quota_device_error gauge
node_xfs_quota_device_error{device="/dev/sda1",device_error="statfs failed",path="/xfs/team-a"} 1
node_xfs_quota_device_error{device="/dev/sdb1",device_error="",path="/xfs/nested/team-b"} 0
`

	if err := testutil.CollectAndCompare(testXFSQuotaCollector{collector}, strings.NewReader(expectedMetrics), "node_xfs_quota_device_error"); err != nil {
		t.Fatal(err)
	}
}

func TestXFSQuotaCollectorNoData(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	collector.mountPointDetails = func(*slog.Logger) ([]filesystemLabels, error) {
		return []filesystemLabels{{device: "/dev/sda1", mountPoint: "/xfs", fsType: "ext4"}}, nil
	}

	if err := collector.Update(make(chan prometheus.Metric)); !IsNoDataError(err) {
		t.Fatalf("unexpected error: got %v, want ErrNoData", err)
	}
}

func TestXFSQuotaCollectorRequiresEnforcedProjectQuota(t *testing.T) {
	collector := newTestXFSQuotaCollector()
	collector.mountPointDetails = func(*slog.Logger) ([]filesystemLabels, error) {
		return []filesystemLabels{{device: "/dev/sda1", mountPoint: "/xfs", fsType: "xfs"}}, nil
	}

	if err := collector.Update(make(chan prometheus.Metric)); !IsNoDataError(err) {
		t.Fatalf("unexpected error: got %v, want ErrNoData", err)
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
	tests := map[string]string{
		"missing separator": "42\n",
		"invalid ID":        "team:/xfs/team\n",
		"relative path":     "42:xfs/team\n",
	}

	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseXFSProjectPaths(strings.NewReader(input)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestXFSMountForPath(t *testing.T) {
	mounts := []filesystemLabels{
		{device: "/dev/sda1", mountPoint: "/xfs", fsType: "xfs"},
		{device: "/dev/sdb1", mountPoint: "/xfs/nested", fsType: "xfs"},
	}

	mount, ok := xfsMountForPath(mounts, "/xfs/nested/team")
	if !ok {
		t.Fatal("expected an XFS mount")
	}
	if got, want := mount.device, "/dev/sdb1"; got != want {
		t.Fatalf("unexpected device: got %q, want %q", got, want)
	}
}

func newTestXFSQuotaCollector() *xfsQuotaCollector {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	createdCollector, err := NewXFSQuotaCollector(logger)
	if err != nil {
		panic(err)
	}
	collector := createdCollector.(*xfsQuotaCollector)
	collector.projectsFile = "/etc/projects"
	collector.mountPointDetails = func(*slog.Logger) ([]filesystemLabels, error) {
		return []filesystemLabels{
			{device: "/dev/sda1", mountPoint: "/xfs", fsType: "xfs", superOptions: "rw,prjquota"},
			{device: "/dev/sdb1", mountPoint: "/xfs/nested", fsType: "xfs", superOptions: "rw,pquota"},
			{device: "/dev/sdc1", mountPoint: "/ext4", fsType: "ext4"},
		}, nil
	}
	collector.readProjectPaths = func(path string) ([]xfsProjectPath, error) {
		if path != "/etc/projects" {
			return nil, errors.New("unexpected projects path")
		}
		return []xfsProjectPath{{id: 42, path: "/xfs/team-a"}}, nil
	}
	collector.statfs = func(path string, stats *unix.Statfs_t) error {
		if path != rootfsFilePath("/xfs/team-a") && path != rootfsFilePath("/xfs/nested/team-b") {
			return errors.New("unexpected statfs path")
		}
		*stats = unix.Statfs_t{
			Bsize:  512,
			Blocks: 20,
			Bfree:  4,
			Bavail: 3,
			Files:  100,
			Ffree:  75,
		}
		return nil
	}

	return collector
}
