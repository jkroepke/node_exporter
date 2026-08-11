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
	"strings"
	"testing"
)

func TestParseXFSQuotaStats(t *testing.T) {
	stats, err := parseXFSQuotaStats(strings.NewReader(`extent_alloc 1 2 3 4
qm 10 11 12 13 14 15 16 17
debug 0
`))
	if err != nil {
		t.Fatal(err)
	}

	expected := xfsQuotaStats{
		dquotReclaims:      10,
		dquotReclaimMisses: 11,
		dquotDuplicates:    12,
		dquotCacheMisses:   13,
		dquotCacheHits:     14,
		dquotWants:         15,
		dquots:             16,
		unusedDquots:       17,
	}
	if stats != expected {
		t.Fatalf("unexpected XFS quota stats: got %+v, want %+v", stats, expected)
	}
}

func TestParseXFSQuotaStatsNotFound(t *testing.T) {
	_, err := parseXFSQuotaStats(strings.NewReader("debug 0\n"))
	if !errors.Is(err, errXFSQuotaStatsNotFound) {
		t.Fatalf("unexpected error: got %v, want %v", err, errXFSQuotaStatsNotFound)
	}
}

func TestParseXFSQuotaStatsInvalid(t *testing.T) {
	tests := map[string]string{
		"incorrect field count": "qm 1 2 3\n",
		"non-numeric field":     "qm 1 2 3 4 5 invalid 7 8\n",
	}

	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseXFSQuotaStats(strings.NewReader(input)); err == nil {
				t.Fatal("expected an error, but none occurred")
			}
		})
	}
}
