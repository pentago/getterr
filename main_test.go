package main

import (
	"encoding/json"
	"io"
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

// Frequent whole-list searches hammer the indexers, and so do very frequent
// batches. The floor also stops a v0.1.1 config like 1m (then a month) from
// searching every minute.
func TestSchedule(t *testing.T) {
	s := searches[0]
	for _, tc := range []struct {
		pace, batch string
		ok          bool
	}{
		{"1m", "", false}, {"59m", "", false}, {"1h", "", true},
		{"9m", "5", false}, {"10m", "5", true},
		{"1d", "0", false}, {"1d", "x", false},
	} {
		t.Setenv("SONARR_MISSING_PACE", tc.pace)
		t.Setenv("SONARR_MISSING_BATCH_SIZE", tc.batch)
		if pace, _, err := s.schedule(); (err == nil) != tc.ok || (err == nil && pace == 0) {
			t.Errorf("pace %q, batch %q: got %v, %v; want ok=%v", tc.pace, tc.batch, pace, err, tc.ok)
		}
	}
	t.Setenv("SONARR_MISSING_PACE", "")
	if pace, _, err := s.schedule(); pace != 0 || err != nil {
		t.Errorf("unset pace: got %v, %v; want disabled", pace, err)
	}
}

// testBatches runs s's batch search of size 2 against handler once per want
// entry and checks what each run searched, as reported through searched.
func testBatches(t *testing.T, s search, handler http.HandlerFunc, searched *[]string, want [][]string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "key" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	defer srv.Close()
	c := conn{srv.Client(), srv.URL, "key"}
	cursor := filepath.Join(t.TempDir(), "cursor")
	for i, w := range want {
		*searched = nil
		if _, err := s.trigger(c, 2, cursor); err != nil || !slices.Equal(*searched, w) {
			t.Errorf("batch %d searched %v, %v; want %v", i+1, *searched, err, w)
		}
	}
}

// Batches walk the Wanted list by ID from a persisted cursor and wrap around,
// so every item gets its turn, even ones Sonarr never marks as searched.
func TestArrBatch(t *testing.T) {
	var searched []string
	testBatches(t, searches[0], func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/wanted/missing" && r.URL.Query().Get("monitored") == "true":
			io.WriteString(w, `{"records":[{"id":5},{"id":1},{"id":3}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/command":
			var cmd struct {
				Name       string
				EpisodeIDs []int
			}
			json.NewDecoder(r.Body).Decode(&cmd)
			for _, id := range cmd.EpisodeIDs {
				searched = append(searched, cmd.Name+"/"+strconv.Itoa(id))
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected request "+r.URL.String(), http.StatusBadRequest)
		}
	}, &searched, [][]string{
		{"EpisodeSearch/1", "EpisodeSearch/3"},
		{"EpisodeSearch/5", "EpisodeSearch/1"}, // wraps around
	})
}

// Bazarr batches search every missing language of each item.
func TestBazarrBatch(t *testing.T) {
	var searched []string
	testBatches(t, searches[4], func(w http.ResponseWriter, r *http.Request) {
		switch {
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
	}, &searched, [][]string{
		{"7/1/en/false/false", "7/1/de/true/false", "7/3/en/false/false"},
		{"9/5/en/false/true", "7/1/en/false/false", "7/1/de/true/false"}, // wraps around
	})
}
