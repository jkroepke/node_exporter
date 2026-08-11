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
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sys/unix"
)

const xfsQuotaSubsystem = "xfs_quota"

var (
	enableXFSQuotaProjectInfo = kingpin.Flag(
		"collector.xfs_quota.project-info",
		"Enables metric node_xfs_quota_project_info using paths from the XFS projects file.",
	).Bool()
	xfsQuotaProjectsPath = kingpin.Flag(
		"collector.xfs_quota.projects-path",
		"Path to the XFS projects file.",
	).Default("/etc/projects").String()
)

type xfsQuotaCollector struct {
	logger             *slog.Logger
	mountPointDetails  func(*slog.Logger) ([]filesystemLabels, error)
	readProjectPaths   func(string) ([]xfsProjectPath, error)
	statfs             func(string, *unix.Statfs_t) error
	projectsFile       string
	projectInfoEnabled bool
	sizeDesc           typedDesc
	freeDesc           typedDesc
	availDesc          typedDesc
	filesDesc          typedDesc
	filesFreeDesc      typedDesc
	projectInfoDesc    typedDesc
}

type xfsProjectPath struct {
	id   uint32
	path string
}

func init() {
	registerCollector("xfs_quota", defaultDisabled, NewXFSQuotaCollector)
}

// NewXFSQuotaCollector returns a new Collector exposing effective XFS project
// quota filesystem statistics through statfs(2).
func NewXFSQuotaCollector(logger *slog.Logger) (Collector, error) {
	quotaLabelNames := []string{"device", "project_id"}

	return &xfsQuotaCollector{
		logger:             logger,
		mountPointDetails:  mountPointDetails,
		readProjectPaths:   readXFSProjectPaths,
		statfs:             unix.Statfs,
		projectsFile:       *xfsQuotaProjectsPath,
		projectInfoEnabled: *enableXFSQuotaProjectInfo,
		sizeDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "size_bytes"),
				"Effective size in bytes reported by statfs for an XFS project quota path.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		freeDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "free_bytes"),
				"Effective free space in bytes reported by statfs for an XFS project quota path.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		availDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "avail_bytes"),
				"Effective space available to non-root users in bytes reported by statfs for an XFS project quota path.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		filesDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "files"),
				"Effective total file nodes reported by statfs for an XFS project quota path.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		filesFreeDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "files_free"),
				"Effective free file nodes reported by statfs for an XFS project quota path.",
				quotaLabelNames,
				nil,
			), valueType: prometheus.GaugeValue,
		},
		projectInfoDesc: typedDesc{
			desc: prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, "project_info"),
				"Information about an XFS project path from the configured projects file.",
				[]string{"device", "project_id", "path"},
				nil,
			), valueType: prometheus.GaugeValue,
		},
	}, nil
}

// Update implements Collector.
func (c *xfsQuotaCollector) Update(ch chan<- prometheus.Metric) error {
	projects, err := c.readProjectPaths(c.projectsFile)
	if err != nil {
		return fmt.Errorf("failed to read XFS project paths from %q: %w", c.projectsFile, err)
	}

	mounts, err := c.mountPointDetails(c.logger)
	if err != nil {
		return fmt.Errorf("failed to retrieve mount points: %w", err)
	}

	xfsMounts := make([]filesystemLabels, 0)
	for _, mount := range mounts {
		if mount.fsType == "xfs" {
			xfsMounts = append(xfsMounts, mount)
		}
	}

	found := false
	emittedQuotas := make(map[string]struct{})
	emittedProjectInfo := make(map[string]struct{})
	for _, project := range projects {
		mount, ok := xfsMountForPath(xfsMounts, project.path)
		if !ok {
			c.logger.Debug("Ignoring XFS project path outside an XFS mount", "project_id", project.id, "path", project.path)
			continue
		}
		if !xfsProjectQuotaEnforced(mount) {
			c.logger.Debug("Ignoring XFS project path on a mount without enforced project quotas", "project_id", project.id, "path", project.path)
			continue
		}

		projectID := strconv.FormatUint(uint64(project.id), 10)
		quotaKey := mount.device + "\x00" + projectID
		if _, ok := emittedQuotas[quotaKey]; !ok {
			stats := new(unix.Statfs_t)
			if err := c.statfs(rootfsFilePath(project.path), stats); err != nil {
				return fmt.Errorf("failed to retrieve XFS project quota for project %s at %q: %w", projectID, project.path, err)
			}

			labelValues := []string{mount.device, projectID}
			blockSize := float64(stats.Bsize)
			ch <- c.sizeDesc.mustNewConstMetric(float64(stats.Blocks)*blockSize, labelValues...)
			ch <- c.freeDesc.mustNewConstMetric(float64(stats.Bfree)*blockSize, labelValues...)
			ch <- c.availDesc.mustNewConstMetric(float64(stats.Bavail)*blockSize, labelValues...)
			ch <- c.filesDesc.mustNewConstMetric(float64(stats.Files), labelValues...)
			ch <- c.filesFreeDesc.mustNewConstMetric(float64(stats.Ffree), labelValues...)

			emittedQuotas[quotaKey] = struct{}{}
			found = true
		}

		if c.projectInfoEnabled {
			infoKey := quotaKey + "\x00" + project.path
			if _, ok := emittedProjectInfo[infoKey]; !ok {
				ch <- c.projectInfoDesc.mustNewConstMetric(1, mount.device, projectID, project.path)
				emittedProjectInfo[infoKey] = struct{}{}
			}
		}
	}

	if !found {
		return ErrNoData
	}

	return nil
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

func xfsProjectQuotaEnforced(mount filesystemLabels) bool {
	for _, options := range []string{mount.mountOptions, mount.superOptions} {
		for option := range strings.SplitSeq(options, ",") {
			if option == "pquota" || option == "prjquota" {
				return true
			}
		}
	}

	return false
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
			return nil, fmt.Errorf("invalid projects entry on line %d", lineNumber)
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
