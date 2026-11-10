package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
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

// A bad batch size must stop getterr: ignoring it would silently turn a
// batch into a whole-list search. Any pace is allowed, by the user's choice.
func TestSchedule(t *testing.T) {
	s := searches[0]
	for _, tc := range []struct {
		pace, batch string
		ok          bool
	}{
		{"1m", "", true}, {"1m", "1", true}, {"1d", "0", false}, {"1d", "x", false},
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

// Startup must stop on problems that won't fix themselves (bad key, auth
// proxy, wrong app) but keep waiting for an app that's still starting.
func TestConnect(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = 0
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)
	reply := func(code int, body string) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Location", "/login")
			w.WriteHeader(code)
			io.WriteString(w, body)
		}
	}
	sonarr := reply(http.StatusOK, `{"appName":"Sonarr","version":"4.0"}`)
	drop := func(w http.ResponseWriter) {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
	}
	for name, tc := range map[string]struct {
		replies []func(http.ResponseWriter)
		ok      bool
	}{
		"sonarr":              {[]func(http.ResponseWriter){sonarr}, true},
		"starting behind 5xx": {[]func(http.ResponseWriter){reply(http.StatusBadGateway, ""), sonarr}, true},
		"unreachable":         {[]func(http.ResponseWriter){drop, sonarr}, true},
		"bad key":             {[]func(http.ResponseWriter){reply(http.StatusUnauthorized, "")}, false},
		"auth proxy redirect": {[]func(http.ResponseWriter){reply(http.StatusFound, "")}, false},
		"login page":          {[]func(http.ResponseWriter){reply(http.StatusOK, "<html>")}, false},
		"other app":           {[]func(http.ResponseWriter){reply(http.StatusOK, `{"appName":"Radarr"}`)}, false},
	} {
		var calls atomic.Int32 // a hijacked connection doesn't sync with the next request
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := int(calls.Add(1)) - 1
			if n >= len(tc.replies) {
				http.Error(w, "too many requests", http.StatusBadRequest)
				return
			}
			tc.replies[n](w)
		}))
		err := connect(searches[0], conn{client, srv.URL, "key"})
		srv.Close()
		if n := int(calls.Load()); (err == nil) != tc.ok || n != len(tc.replies) {
			t.Errorf("%s: connect = %v after %d requests; want ok=%v after %d", name, err, n, tc.ok, len(tc.replies))
		}
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
			// Pages like Sonarr, with a default page smaller than the list,
			// so a request without a big enough pageSize gets a partial list.
			records := []map[string]int{{"id": 5}, {"id": 1}, {"id": 3}}
			size, err := strconv.Atoi(r.URL.Query().Get("pageSize"))
			if err != nil {
				size = 2
			}
			json.NewEncoder(w).Encode(map[string]any{"totalRecords": len(records), "records": records[:min(size, len(records))]})
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
