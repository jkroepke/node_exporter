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
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

const xfsQuotaSubsystem = "xfs_quota"

var errXFSQuotaStatsNotFound = errors.New("XFS quota manager stats not found")

type xfsQuotaStats struct {
	dquotReclaims      uint32
	dquotReclaimMisses uint32
	dquotDuplicates    uint32
	dquotCacheMisses   uint32
	dquotCacheHits     uint32
	dquotWants         uint32
	dquots             uint32
	unusedDquots       uint32
}

type xfsQuotaCollector struct {
	statsFile string
	logger    *slog.Logger
}

func init() {
	registerCollector("xfs_quota", defaultDisabled, NewXFSQuotaCollector)
}

// NewXFSQuotaCollector returns a new Collector exposing XFS quota manager statistics.
func NewXFSQuotaCollector(logger *slog.Logger) (Collector, error) {
	return &xfsQuotaCollector{
		statsFile: procFilePath("fs/xfs/stat"),
		logger:    logger,
	}, nil
}

func parseXFSQuotaStats(r io.Reader) (xfsQuotaStats, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "qm" {
			continue
		}
		if len(fields) != 9 {
			return xfsQuotaStats{}, fmt.Errorf("unexpected number of XFS quota manager stats: %d", len(fields)-1)
		}

		values := make([]uint32, 8)
		for i, field := range fields[1:] {
			value, err := strconv.ParseUint(field, 10, 32)
			if err != nil {
				return xfsQuotaStats{}, fmt.Errorf("invalid XFS quota manager stat %q: %w", field, err)
			}
			values[i] = uint32(value)
		}

		return xfsQuotaStats{
			dquotReclaims:      values[0],
			dquotReclaimMisses: values[1],
			dquotDuplicates:    values[2],
			dquotCacheMisses:   values[3],
			dquotCacheHits:     values[4],
			dquotWants:         values[5],
			dquots:             values[6],
			unusedDquots:       values[7],
		}, nil
	}
	if err := scanner.Err(); err != nil {
		return xfsQuotaStats{}, err
	}

	return xfsQuotaStats{}, errXFSQuotaStatsNotFound
}

// Update implements Collector.
func (c *xfsQuotaCollector) Update(ch chan<- prometheus.Metric) error {
	file, err := os.Open(c.statsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.logger.Debug("XFS quota manager statistics are unavailable", "err", err)
			return ErrNoData
		}
		return fmt.Errorf("failed to open XFS stats: %w", err)
	}
	defer file.Close()

	stats, err := parseXFSQuotaStats(file)
	if errors.Is(err, errXFSQuotaStatsNotFound) {
		c.logger.Debug("XFS quota manager statistics are unavailable", "err", err)
		return ErrNoData
	}
	if err != nil {
		return fmt.Errorf("failed to parse XFS quota manager stats: %w", err)
	}

	metrics := []struct {
		name      string
		help      string
		value     uint32
		valueType prometheus.ValueType
	}{
		{"dquot_reclaims_total", "Number of XFS dquots reclaimed.", stats.dquotReclaims, prometheus.CounterValue},
		{"dquot_reclaim_misses_total", "Number of missed XFS dquot reclaim attempts.", stats.dquotReclaimMisses, prometheus.CounterValue},
		{"dquot_duplicates_total", "Number of duplicate XFS dquots found.", stats.dquotDuplicates, prometheus.CounterValue},
		{"dquot_cache_misses_total", "Number of XFS dquot cache misses.", stats.dquotCacheMisses, prometheus.CounterValue},
		{"dquot_cache_hits_total", "Number of XFS dquot cache hits.", stats.dquotCacheHits, prometheus.CounterValue},
		{"dquot_wants_total", "Number of XFS dquot wants.", stats.dquotWants, prometheus.CounterValue},
		{"dquots", "Number of XFS dquots currently in core.", stats.dquots, prometheus.GaugeValue},
		{"unused_dquots", "Number of unused XFS dquots on the freelist.", stats.unusedDquots, prometheus.GaugeValue},
	}

	for _, metric := range metrics {
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				prometheus.BuildFQName(namespace, xfsQuotaSubsystem, metric.name),
				metric.help,
				nil,
				nil,
			),
			metric.valueType,
			float64(metric.value),
		)
	}

	return nil
}
