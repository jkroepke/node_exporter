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
	"log/slog"
	"strconv"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

const (
	xfsQuotaSubsystem = "xfs_quota"
	xfsQuotaBlockSize = 512

	// XFS quota commands from include/uapi/linux/dqblk_xfs.h.
	qXGetNextQuota        = ('X' << 8) + 9
	xqmProjectQuota       = 2
	qXGetNextProjectQuota = (qXGetNextQuota << 8) | xqmProjectQuota
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
}

func init() {
	registerCollector("xfs_quota", defaultDisabled, NewXFSQuotaCollector)
}

// NewXFSQuotaCollector returns a new Collector exposing XFS project quotas.
func NewXFSQuotaCollector(logger *slog.Logger) (Collector, error) {
	return &xfsQuotaCollector{
		logger:              logger,
		mountPointDetails:   mountPointDetails,
		getNextProjectQuota: getNextXFSProjectQuota,
	}, nil
}

// Update implements Collector.
func (c *xfsQuotaCollector) Update(ch chan<- prometheus.Metric) error {
	mounts, err := c.mountPointDetails(c.logger)
	if err != nil {
		return fmt.Errorf("failed to retrieve mount points: %w", err)
	}

	devices := make(map[string]struct{})
	found := false

	for _, mount := range mounts {
		if mount.fsType != "xfs" {
			continue
		}
		if _, ok := devices[mount.device]; ok {
			continue
		}
		devices[mount.device] = struct{}{}

		devicePath := rootfsFilePath(mount.device)
		for projectID := uint32(0); ; {
			quota, err := c.getNextProjectQuota(devicePath, projectID)
			if errors.Is(err, unix.ESRCH) {
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

	if !found {
		return ErrNoData
	}

	return nil
}

func (c *xfsQuotaCollector) updateXFSProjectQuota(ch chan<- prometheus.Metric, device string, quota xfsDiskQuota) {
	labels := []string{"device", "project_id"}
	labelValues := []string{device, strconv.FormatUint(uint64(quota.ID), 10)}

	metrics := []struct {
		name  string
		desc  string
		value float64
	}{
		{
			name:  "used_bytes",
			desc:  "Number of bytes used by an XFS project quota.",
			value: float64(quota.BlockCount) * xfsQuotaBlockSize,
		},
		{
			name:  "soft_limit_bytes",
			desc:  "Soft limit in bytes for an XFS project quota.",
			value: float64(quota.BlockSoftLimit) * xfsQuotaBlockSize,
		},
		{
			name:  "hard_limit_bytes",
			desc:  "Hard limit in bytes for an XFS project quota.",
			value: float64(quota.BlockHardLimit) * xfsQuotaBlockSize,
		},
		{
			name:  "used_inodes",
			desc:  "Number of inodes used by an XFS project quota.",
			value: float64(quota.InodeCount),
		},
		{
			name:  "soft_limit_inodes",
			desc:  "Soft inode limit for an XFS project quota.",
			value: float64(quota.InodeSoftLimit),
		},
		{
			name:  "hard_limit_inodes",
			desc:  "Hard inode limit for an XFS project quota.",
			value: float64(quota.InodeHardLimit),
		},
	}

	for _, metric := range metrics {
		desc := prometheus.NewDesc(
			prometheus.BuildFQName(namespace, xfsQuotaSubsystem, metric.name),
			metric.desc,
			labels,
			nil,
		)

		ch <- prometheus.MustNewConstMetric(
			desc,
			prometheus.GaugeValue,
			metric.value,
			labelValues...,
		)
	}
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
