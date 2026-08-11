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
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

const (
	xfsQuotaSubsystem = "xfs_quota"
	xfsQuotaBlockSize = 512

	// XFS quota commands from include/uapi/linux/dqblk_xfs.h.
	qXGetNextQuota        = uintptr(('X' << 8) + 9)
	xqmProjectQuota       = uintptr(2)
	qXGetNextProjectQuota = (qXGetNextQuota << 8) | xqmProjectQuota
)

var (
	enableXFSQuotaProjectInfo = kingpin.Flag(
		"collector.xfs_quota.project-info",
		"Enables metric node_xfs_quota_project_info using project paths from /etc/projects.",
	).Bool()
	xfsQuotaProjectsPath = kingpin.Flag(
		"collector.xfs_quota.projects-path",
		"Path to the XFS projects file used by the project-info subcollector.",
	).Default("/etc/projects").String()
)

// xfsDiskQuota matches struct fs_disk_quota from
// include/uapi/linux/dqblk_xfs.h.
type xfsDiskQuota struct {
	Version                int8
	Flags                  int8
	FieldMask              uint16
	ID                     uint32
	BlockHardLimit         uint64
	BlockSoftLimit         uint64
	InodeHardLimit         uint64
	InodeSoftLimit         uint64
	BlockCount             uint64
	InodeCount             uint64
	InodeTimer             int32
	BlockTimer             int32
	InodeWarnings          uint16
	BlockWarnings          uint16
	InodeTimerHigh         int8
	BlockTimerHigh         int8
	RealtimeBlockTimerHigh int8
	Padding2               int8
	RealtimeBlockHardLimit uint64
	RealtimeBlockSoftLimit uint64
	RealtimeBlockCount     uint64
	RealtimeBlockTimer     int32
	RealtimeBlockWarnings  uint16
	Padding3               int16
	Padding4               [8]byte
}

type xfsQuotaCollector struct {
	logger              *slog.Logger
	mountPointDetails   func(*slog.Logger) ([]filesystemLabels, error)
	getNextProjectQuota func(string, uint32) (xfsDiskQuota, error)
	readProjectPaths    func(string) ([]xfsProjectPath, error)
	projectsFile        string
	projectInfoEnabled  bool
	usedBytesDesc       typedDesc
	softLimitBytesDesc  typedDesc
	hardLimitBytesDesc  typedDesc
	usedInodesDesc      typedDesc
	softLimitInodesDesc typedDesc
	hardLimitInodesDesc typedDesc
	projectInfoDesc     typedDesc
}

type xfsProjectPath struct {
	id   uint32
	path string
}

func init() {
	registerCollector("xfs_quota", defaultDisabled, NewXFSQuotaCollector)
}

// NewXFSQuotaCollector returns a new Collector exposing XFS project quotas.
func NewXFSQuotaCollector(logger *slog.Logger) (Collector, error) {
	quotaLabelNames := []string{"device", "project_id"}

	return &xfsQuotaCollector{
		logger:              logger,
		mountPointDetails:   mountPointDetails,
		getNextProjectQuota: getNextXFSProjectQuota,
		readProjectPaths:    readXFSProjectPaths,
		projectsFile:        *xfsQuotaProjectsPath,
		projectInfoEnabled:  *enableXFSQuotaProjectInfo,
		usedBytesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "used_bytes"),
				"Number of bytes used by an XFS project quota.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		softLimitBytesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "soft_limit_bytes"),
				"Soft limit in bytes for an XFS project quota.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		hardLimitBytesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "hard_limit_bytes"),
				"Hard limit in bytes for an XFS project quota.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		usedInodesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "used_inodes"),
				"Number of inodes used by an XFS project quota.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		softLimitInodesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "soft_limit_inodes"),
				"Soft inode limit for an XFS project quota.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		hardLimitInodesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "hard_limit_inodes"),
				"Hard inode limit for an XFS project quota.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		projectInfoDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "project_info"),
				"Information about an XFS project path from /etc/projects.",
				[]string{"device", "project_id", "path"},
				nil,
			), valueType: prometheus.GaugeValue,
		},
	}, nil
}

// Update implements Collector.
func (c *xfsQuotaCollector) Update(ch chan<- prometheus.Metric) error {
	mounts, err := c.mountPointDetails(c.logger)
	if err != nil {
		return fmt.Errorf("failed to retrieve mount points: %w", err)
	}

	devices := make(map[string]struct{})
	xfsMounts := make([]filesystemLabels, 0)
	found := false

	for _, mount := range mounts {
		if mount.fsType != "xfs" {
			continue
		}
		xfsMounts = append(xfsMounts, mount)
		if _, ok := devices[mount.device]; ok {
			continue
		}
		devices[mount.device] = struct{}{}

		devicePath := rootfsFilePath(mount.device)
		for projectID := uint32(0); ; {
			quota, err := c.getNextProjectQuota(devicePath, projectID)
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				break
			}
			if err != nil {
				return fmt.Errorf("failed to retrieve XFS project quota for device %q: %w", mount.device, err)
			}

			c.updateXFSProjectQuota(ch, mount.device, quota)
			found = true

			if quota.ID == ^uint32(0) {
				break
			}
			projectID = quota.ID + 1
		}
	}

	if c.projectInfoEnabled {
		projectInfoFound, err := c.updateXFSProjectInfo(ch, xfsMounts)
		if err != nil {
			return err
		}
		found = found || projectInfoFound
	}

	if !found {
		return ErrNoData
	}

	return nil
}

func (c *xfsQuotaCollector) updateXFSProjectQuota(ch chan<- prometheus.Metric, device string, quota xfsDiskQuota) {
	labelValues := []string{device, strconv.FormatUint(uint64(quota.ID), 10)}

	ch <- c.usedBytesDesc.mustNewConstMetric(float64(quota.BlockCount)*xfsQuotaBlockSize, labelValues...)
	ch <- c.softLimitBytesDesc.mustNewConstMetric(float64(quota.BlockSoftLimit)*xfsQuotaBlockSize, labelValues...)
	ch <- c.hardLimitBytesDesc.mustNewConstMetric(float64(quota.BlockHardLimit)*xfsQuotaBlockSize, labelValues...)
	ch <- c.usedInodesDesc.mustNewConstMetric(float64(quota.InodeCount), labelValues...)
	ch <- c.softLimitInodesDesc.mustNewConstMetric(float64(quota.InodeSoftLimit), labelValues...)
	ch <- c.hardLimitInodesDesc.mustNewConstMetric(float64(quota.InodeHardLimit), labelValues...)
}

func (c *xfsQuotaCollector) updateXFSProjectInfo(ch chan<- prometheus.Metric, mounts []filesystemLabels) (bool, error) {
	projects, err := c.readProjectPaths(c.projectsFile)
	if errors.Is(err, os.ErrNotExist) {
		c.logger.Debug("XFS projects file does not exist", "path", c.projectsFile)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to read XFS project paths from %q: %w", c.projectsFile, err)
	}

	found := false
	emitted := make(map[string]struct{})
	for _, project := range projects {
		mount, ok := xfsMountForPath(mounts, project.path)
		if !ok {
			continue
		}

		projectID := strconv.FormatUint(uint64(project.id), 10)
		key := mount.device + "\x00" + projectID + "\x00" + project.path
		if _, ok := emitted[key]; ok {
			continue
		}
		emitted[key] = struct{}{}

		ch <- c.projectInfoDesc.mustNewConstMetric(1, mount.device, projectID, project.path)
		found = true
	}

	return found, nil
}

func xfsMountForPath(mounts []filesystemLabels, path string) (filesystemLabels, bool) {
	var selected filesystemLabels
	for _, mount := range mounts {
		relativePath, err := filepath.Rel(mount.mountPoint, path)
		if err != nil || relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
			continue
		}
		if selected.mountPoint == "" || len(mount.mountPoint) > len(selected.mountPoint) {
			selected = mount
		}
	}

	return selected, selected.mountPoint != ""
}

func readXFSProjectPaths(path string) ([]xfsProjectPath, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return parseXFSProjectPaths(file)
}

func parseXFSProjectPaths(reader io.Reader) ([]xfsProjectPath, error) {
	projects := make([]xfsProjectPath, 0)
	scanner := bufio.NewScanner(reader)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		projectID, projectPath, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("invalid /etc/projects entry on line %d", lineNumber)
		}

		id, err := strconv.ParseUint(strings.TrimSpace(projectID), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid project ID on line %d: %w", lineNumber, err)
		}

		projectPath = strings.TrimSpace(projectPath)
		if !filepath.IsAbs(projectPath) {
			return nil, fmt.Errorf("project path on line %d is not absolute", lineNumber)
		}

		projects = append(projects, xfsProjectPath{
			id:   uint32(id),
			path: strings.ToValidUTF8(filepath.Clean(projectPath), "�"),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return projects, nil
}

func getNextXFSProjectQuota(device string, projectID uint32) (xfsDiskQuota, error) {
	devicePointer, err := unix.BytePtrFromString(device)
	if err != nil {
		return xfsDiskQuota{}, err
	}

	quota := xfsDiskQuota{}
	_, _, errno := unix.Syscall6(
		unix.SYS_QUOTACTL,
		qXGetNextProjectQuota,
		uintptr(unsafe.Pointer(devicePointer)),
		uintptr(projectID),
		uintptr(unsafe.Pointer(&quota)),
		0,
		0,
	)
	if errno != 0 {
		return xfsDiskQuota{}, errno
	}

	return quota, nil
}
