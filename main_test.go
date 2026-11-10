package main

import (
	"cmp"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestParsePace(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"10m": 10 * time.Minute, "6h": 6 * time.Hour, "1d": day, "2w": 14 * day, "1M": 30 * day,
	} {
		if got, err := parsePace(in); err != nil || got != want {
			t.Errorf("parsePace(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "h", "0h", "-1d", "1y", "1.5h", "1H", "99999999999M"} {
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

// Bazarr's wanted list has no last search time, so batches walk it by ID from
// a persisted cursor and wrap around, searching every missing language.
func TestBazarrBatch(t *testing.T) {
	var searched []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("X-Api-Key") != "key":
			http.Error(w, "bad key", http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/api/episodes/wanted":
			io.WriteString(w, `{"data":[
				{"sonarrSeriesId":9,"sonarrEpisodeId":5,"missing_subtitles":[{"code2":"en","forced":false,"hi":true}]},
				{"sonarrSeriesId":7,"sonarrEpisodeId":1,"missing_subtitles":[{"code2":"en","forced":false,"hi":false},{"code2":"de","forced":true,"hi":false}]},
				{"sonarrSeriesId":7,"sonarrEpisodeId":3,"missing_subtitles":[{"code2":"en","forced":false,"hi":false}]}]}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/api/episodes/subtitles":
			q := r.URL.Query()
			searched = append(searched, q.Get("seriesid")+"/"+q.Get("episodeid")+"/"+q.Get("language")+"/"+q.Get("forced")+"/"+q.Get("hi"))
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	j := job{"bazarr", "wanted_search_missing_subtitles_series", "BAZARR_SERIES_PACE", "BAZARR_SERIES_BATCH_SIZE", "episodes", "searching missing episode subtitles"}
	cursor := filepath.Join(t.TempDir(), "cursor")
	for i, want := range [][]string{
		{"7/1/en/false/false", "7/1/de/true/false", "7/3/en/false/false"},
		{"9/5/en/false/true", "7/1/en/false/false", "7/1/de/true/false"}, // wraps around
	} {
		searched = nil
		if _, err := bazarrBatch(srv.Client(), j, srv.URL, "key", 2, cursor); err != nil || !slices.Equal(searched, want) {
			t.Errorf("batch %d searched %v, %v; want %v", i+1, searched, err, want)
		}
	}
}
