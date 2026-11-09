package main

import (
	"cmp"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestParsePace(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"6h": 6 * time.Hour, "1d": day, "2w": 14 * day, "1m": 30 * day,
	} {
		if got, err := parsePace(in); err != nil || got != want {
			t.Errorf("parsePace(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "h", "0h", "-1d", "1y", "1.5h", "1H", "99999999999m"} {
		if _, err := parsePace(in); err == nil {
			t.Errorf("parsePace(%q) succeeded; want error", in)
		}
	}
}

// Sonarr/Radarr pass sortDirection straight to SQL, so never-searched items
// (null lastSearchTime) come first on SQLite but last on Postgres. On both,
// never-searched items must go first, then the least recently searched.
func TestOldestSearched(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { v := base.Add(time.Duration(h) * time.Hour); return &v }
	type record struct {
		ID             int        `json:"id"`
		LastSearchTime *time.Time `json:"lastSearchTime"`
	}
	all := []record{{1, at(3)}, {2, nil}, {3, at(1)}, {4, at(4)}, {5, nil}, {6, at(2)}}

	for db, nullKey := range map[string]float64{"sqlite": math.Inf(-1), "postgres": math.Inf(1)} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if r.URL.Path != "/api/v3/wanted/missing" || q.Get("sortKey") != "episodes.lastSearchTime" || r.Header.Get("X-Api-Key") != "key" {
				http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
				return
			}
			key := func(r record) float64 {
				if r.LastSearchTime == nil {
					return nullKey
				}
				return float64(r.LastSearchTime.Unix())
			}
			sorted := slices.Clone(all)
			slices.SortFunc(sorted, func(a, b record) int { return cmp.Compare(key(a), key(b)) })
			if q.Get("sortDirection") == "descending" {
				slices.Reverse(sorted)
			}
			size, _ := strconv.Atoi(q.Get("pageSize"))
			json.NewEncoder(w).Encode(map[string]any{"records": sorted[:min(size, len(sorted))]})
		}))
		for n, want := range map[int][]int{2: {2, 5}, 3: {2, 5, 3}, 5: {2, 5, 3, 6, 1}, 9: {2, 5, 3, 6, 1, 4}} {
			got, err := oldestSearched(srv.Client(), srv.URL, "key", "missing", "episodes.lastSearchTime", n)
			if err != nil || !slices.Equal(got, want) {
				t.Errorf("%s: oldestSearched(n=%d) = %v, %v; want %v", db, n, got, err, want)
			}
		}
		srv.Close()
	}
}
