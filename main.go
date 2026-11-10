// Getterr periodically triggers Sonarr/Radarr "Wanted" searches
// (missing + cutoff unmet) via their /api/v3/command endpoint, and Bazarr's
// missing subtitles searches via its /api.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const retryDelay = 5 * time.Minute

// statusError is an HTTP reply with an unexpected status code.
type statusError int

func (e statusError) Error() string {
	return fmt.Sprintf("HTTP %d %s", int(e), http.StatusText(int(e)))
}

type job struct {
	app, command, paceVar, batchVar, wanted, desc string
}

// String is the log prefix, e.g. "sonarr: searching missing episodes".
func (j job) String() string { return j.app + ": " + j.desc }

var jobs = []job{
	{"sonarr", "MissingEpisodeSearch", "SONARR_MISSING_PACE", "SONARR_BATCH_SIZE", "missing", "searching missing episodes"},
	{"sonarr", "CutoffUnmetEpisodeSearch", "SONARR_UNMET_PACE", "SONARR_BATCH_SIZE", "cutoff", "searching cutoff unmet episodes"},
	{"radarr", "MissingMoviesSearch", "RADARR_MISSING_PACE", "RADARR_BATCH_SIZE", "missing", "searching missing movies"},
	{"radarr", "CutoffUnmetMoviesSearch", "RADARR_UNMET_PACE", "RADARR_BATCH_SIZE", "cutoff", "searching cutoff unmet movies"},
	{"bazarr", "wanted_search_missing_subtitles_series", "BAZARR_SERIES_PACE", "BAZARR_SERIES_BATCH_SIZE", "episodes", "searching missing episode subtitles"},
	{"bazarr", "wanted_search_missing_subtitles_movies", "BAZARR_MOVIES_PACE", "BAZARR_MOVIES_BATCH_SIZE", "movies", "searching missing movie subtitles"},
}

// batches says how Sonarr/Radarr search a batchVar-sized batch of wanted items by ID.
var batches = map[string]struct{ sortKey, command, idsField, items string }{
	"sonarr": {"episodes.lastSearchTime", "EpisodeSearch", "episodeIds", "episodes"},
	"radarr": {"movies.lastSearchTime", "MoviesSearch", "movieIds", "movies"},
}

var units = map[byte]time.Duration{
	'm': time.Minute,
	'h': time.Hour,
	'd': 24 * time.Hour,
	'w': 7 * 24 * time.Hour,
	'M': 30 * 24 * time.Hour,
}

// parsePace parses "<N><unit>" with unit m (minutes), h (hours), d (days), w (weeks), M (30-day months).
func parsePace(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid pace %q", s)
	}
	unit, ok := units[s[len(s)-1]]
	n, err := strconv.Atoi(s[:len(s)-1])
	if !ok || err != nil || n <= 0 || time.Duration(n) > math.MaxInt64/unit {
		return 0, fmt.Errorf("invalid pace %q: want <N>m, <N>h, <N>d, <N>w or <N>M", s)
	}
	return time.Duration(n) * unit, nil
}

// api sends a JSON request and decodes the JSON reply into out, if non-nil.
// Only the want status counts as success: anything else (e.g. an auth proxy's
// redirect or login page) means the request never reached Sonarr/Radarr.
func api(client *http.Client, method, url, apiKey string, in, out any, want int) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return statusError(resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// trigger queues j's search over the whole Wanted list or, when batch > 0,
// over a batch of batch items. It returns a log message.
func trigger(client *http.Client, j job, baseURL, apiKey string, batch int, statePath string) (string, error) {
	if j.app == "bazarr" {
		if batch > 0 {
			return bazarrBatch(client, j, baseURL, apiKey, batch, statePath+".cursor")
		}
		// Bazarr answers a started task with 204.
		return "queued", api(client, http.MethodPost, baseURL+"/api/system/tasks?taskid="+j.command, apiKey, nil, nil, http.StatusNoContent)
	}
	cmd, msg := map[string]any{"name": j.command}, "queued"
	if batch > 0 {
		b := batches[j.app]
		ids, err := oldestSearched(client, baseURL, apiKey, j.wanted, b.sortKey, batch)
		if err != nil {
			return "", err
		}
		if len(ids) == 0 {
			return "nothing to search", nil
		}
		cmd = map[string]any{"name": b.command, b.idsField: ids}
		msg = fmt.Sprintf("queued search for %d %s", len(ids), b.items)
	}
	// Sonarr/Radarr answer a queued command with 201.
	return msg, api(client, http.MethodPost, baseURL+"/api/v3/command", apiKey, cmd, nil, http.StatusCreated)
}

// oldestSearched returns up to n IDs from the "missing" or "cutoff" Wanted
// list: never searched first, then least recently searched. Sonarr/Radarr sort
// a null lastSearchTime first on SQLite but last on Postgres, so it reads n
// items from each end of the sort; on either database the n oldest are among them.
func oldestSearched(client *http.Client, baseURL, apiKey, wanted, sortKey string, n int) ([]int, error) {
	type record struct {
		ID             int       `json:"id"`
		LastSearchTime time.Time `json:"lastSearchTime"` // zero if never searched
	}
	var records []record
	for _, dir := range []string{"ascending", "descending"} {
		var page struct{ Records []record }
		url := fmt.Sprintf("%s/api/v3/wanted/%s?monitored=true&pageSize=%d&sortKey=%s&sortDirection=%s", baseURL, wanted, n, sortKey, dir)
		if err := api(client, http.MethodGet, url, apiKey, nil, &page, http.StatusOK); err != nil {
			return nil, err
		}
		records = append(records, page.Records...)
	}
	slices.SortFunc(records, func(a, b record) int {
		return cmp.Or(a.LastSearchTime.Compare(b.LastSearchTime), cmp.Compare(a.ID, b.ID))
	})
	records = slices.CompactFunc(records, func(a, b record) bool { return a.ID == b.ID })
	ids := make([]int, 0, n)
	for _, r := range records[:min(n, len(records))] {
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// bazarrBatch queues a subtitle search for every missing language of the n
// wanted episodes or movies that follow the last batch by ID, wrapping around.
// Bazarr's wanted list has no last search time, so the last ID searched is
// kept in cursorPath. A failed cursor write is only logged.
func bazarrBatch(client *http.Client, j job, baseURL, apiKey string, n int, cursorPath string) (string, error) {
	type item struct {
		SeriesID  int `json:"sonarrSeriesId"`
		EpisodeID int `json:"sonarrEpisodeId"`
		MovieID   int `json:"radarrId"`
		Missing   []struct {
			Code2      string
			Forced, HI bool
		} `json:"missing_subtitles"`
	}
	id := func(it item) int {
		if j.wanted == "movies" {
			return it.MovieID
		}
		return it.EpisodeID
	}
	var page struct{ Data []item }
	if err := api(client, http.MethodGet, baseURL+"/api/"+j.wanted+"/wanted", apiKey, nil, &page, http.StatusOK); err != nil {
		return "", err
	}
	items := page.Data
	slices.SortFunc(items, func(a, b item) int { return cmp.Compare(id(a), id(b)) })
	b, _ := os.ReadFile(cursorPath)
	cursor, _ := strconv.Atoi(strings.TrimSpace(string(b))) // 0 if missing
	i, _ := slices.BinarySearchFunc(items, cursor+1, func(it item, t int) int { return cmp.Compare(id(it), t) })
	items = slices.Concat(items[i:], items[:i])[:min(n, len(items))]
	if len(items) == 0 {
		return "nothing to search", nil
	}
	searches := 0
	for _, it := range items {
		for _, l := range it.Missing {
			q := url.Values{"language": {l.Code2}, "forced": {strconv.FormatBool(l.Forced)}, "hi": {strconv.FormatBool(l.HI)}}
			if j.wanted == "movies" {
				q.Set("radarrid", strconv.Itoa(it.MovieID))
			} else {
				q.Set("seriesid", strconv.Itoa(it.SeriesID))
				q.Set("episodeid", strconv.Itoa(it.EpisodeID))
			}
			// Bazarr queues the search and answers 204.
			if err := api(client, http.MethodPatch, baseURL+"/api/"+j.wanted+"/subtitles?"+q.Encode(), apiKey, nil, nil, http.StatusNoContent); err != nil {
				return "", err
			}
			searches++
		}
	}
	if err := os.WriteFile(cursorPath, []byte(strconv.Itoa(id(items[len(items)-1]))), 0o644); err != nil {
		log.Printf("%s - cannot persist batch position: %v", j, err)
	}
	return fmt.Sprintf("queued %d subtitles for %d %s", searches, len(items), j.wanted), nil
}

// connect waits until baseURL answers as j.app and accepts apiKey. A rejected
// key, an auth proxy or a URL pointing at another app won't fix itself, so
// those are fatal. An unreachable app or a proxy's 5xx may just be starting up
// (or be a mistyped address, which looks the same), so those are retried.
func connect(client *http.Client, j job, baseURL, apiKey string) {
	prefix := strings.ToUpper(j.app)
	for {
		var status struct{ AppName, Version string }
		var err error
		if j.app == "bazarr" {
			// Bazarr's status has no appName, and only Bazarr reports bazarr_version.
			var s struct {
				Data struct {
					Version string `json:"bazarr_version"`
				}
			}
			err = api(client, http.MethodGet, baseURL+"/api/system/status", apiKey, nil, &s, http.StatusOK)
			if s.Data.Version != "" {
				status.AppName, status.Version = "Bazarr", s.Data.Version
			}
		} else {
			err = api(client, http.MethodGet, baseURL+"/api/v3/system/status", apiKey, nil, &status, http.StatusOK)
		}
		if err == nil && strings.EqualFold(status.AppName, j.app) {
			log.Printf("%s - connected to %s %s at %s", j, status.AppName, status.Version, baseURL)
			return
		}
		if err == nil {
			log.Fatalf("%s - %s answers as %q, not %s; check %s_URL", j, baseURL, status.AppName, j.app, prefix)
		}
		var netErr *url.Error
		var code statusError
		if !errors.As(err, &netErr) && !(errors.As(err, &code) && code >= 500) {
			log.Fatalf("%s - %s rejected the request, check %s_URL and %s_API_KEY: %v", j, baseURL, prefix, prefix, err)
		}
		log.Printf("%s - cannot reach %s, retrying in %s: %v", j, baseURL, retryDelay, err)
		time.Sleep(retryDelay)
	}
}

func main() {
	stateDir := os.Getenv("STATE_DIR")
	if stateDir == "" {
		stateDir = "/data"
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		// Never follow redirects: Go would forward X-Api-Key to the target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	started := 0
	for _, j := range jobs {
		raw := os.Getenv(j.paceVar)
		if raw == "" {
			log.Printf("%s - disabled (%s unset)", j, j.paceVar)
			continue
		}
		pace, err := parsePace(raw)
		if err != nil {
			log.Fatalf("%s - %s: %v", j, j.paceVar, err)
		}
		prefix := strings.ToUpper(j.app)
		baseURL := strings.TrimRight(os.Getenv(prefix+"_URL"), "/")
		apiKey := os.Getenv(prefix + "_API_KEY")
		if baseURL == "" || apiKey == "" {
			log.Fatalf("%s - %s is set but %s_URL or %s_API_KEY is missing", j, j.paceVar, prefix, prefix)
		}
		batch := 0
		if raw := os.Getenv(j.batchVar); raw != "" {
			if batch, err = strconv.Atoi(raw); err != nil || batch <= 0 {
				log.Fatalf("%s - %s: invalid %q, want a positive number", j, j.batchVar, raw)
			}
		}
		// SONARR_MISSING_PACE -> sonarr-missing
		state := strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(j.paceVar, "_PACE"), "_", "-"))
		go run(client, j, pace, batch, baseURL, apiKey, filepath.Join(stateDir, state))
		started++
	}
	if started == 0 {
		log.Fatal("no *_PACE variable set, nothing to do")
	}
	select {}
}

// run checks the app is reachable, then triggers the job every pace. The last
// successful run time is persisted so container restarts neither re-run early
// nor skip long (e.g. monthly) paces.
func run(client *http.Client, j job, pace time.Duration, batch int, baseURL, apiKey, statePath string) {
	connect(client, j, baseURL, apiKey)
	var last time.Time
	if b, err := os.ReadFile(statePath); err == nil {
		last, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	}
	for {
		next := last.Add(pace)
		if wait := time.Until(next); wait > 0 {
			log.Printf("%s - next run at %s", j, next.Format(time.RFC3339))
			time.Sleep(wait)
		}
		msg, err := trigger(client, j, baseURL, apiKey, batch, statePath)
		if err != nil {
			log.Printf("%s - failed, retrying in %s: %v", j, retryDelay, err)
			time.Sleep(retryDelay)
			continue
		}
		log.Printf("%s - %s", j, msg)
		last = time.Now()
		if err := os.WriteFile(statePath, []byte(last.Format(time.RFC3339)), 0o644); err != nil {
			log.Printf("%s - cannot persist last run: %v", j, err)
		}
	}
}
