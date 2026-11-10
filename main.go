// Getterr periodically triggers Sonarr/Radarr "Wanted" searches (missing and
// cutoff unmet) and Bazarr's missing subtitles searches, either over a whole
// Wanted list or a batch of it at a time.
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

// conn is the API of the app at url, which accepts key.
type conn struct {
	client   *http.Client
	url, key string
}

// do sends a JSON request to path and decodes the JSON reply into out, if non-nil.
// Only the want status counts as success: anything else (e.g. an auth proxy's
// redirect or login page) means the request never reached the app.
func (c conn) do(method, path string, in, out any, want int) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
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

// app is the part of the Sonarr, Radarr or Bazarr API that getterr uses.
type app interface {
	// status returns the name and version the app reports.
	status(c conn) (name, version string, err error)
	// searchAll queues the app's own search over a whole Wanted list.
	searchAll(c conn, command string) error
	// searchBatch queues a search for the n items of a Wanted list that follow
	// the item with ID cursor. It returns the last ID searched and a log message.
	searchBatch(c conn, list string, cursor, n int) (last int, msg string, err error)
}

// next sorts items by ID and returns up to n of them, starting after the one
// with ID cursor and wrapping around.
func next[T any](items []T, id func(T) int, cursor, n int) []T {
	slices.SortFunc(items, func(a, b T) int { return cmp.Compare(id(a), id(b)) })
	i, _ := slices.BinarySearchFunc(items, cursor+1, func(it T, t int) int { return cmp.Compare(id(it), t) })
	return slices.Concat(items[i:], items[:i])[:min(n, len(items))]
}

// arr is Sonarr or Radarr. API v3 works on Sonarr v4/v5 and Radarr v6/v7.
type arr struct {
	items    string // for logs
	command  string // searches items by ID
	idsField string // the command's ID list
}

func (arr) status(c conn) (string, string, error) {
	var s struct{ AppName, Version string }
	err := c.do(http.MethodGet, "/api/v3/system/status", nil, &s, http.StatusOK)
	return s.AppName, s.Version, err
}

// Sonarr/Radarr answer a queued command with 201.
func (arr) searchAll(c conn, command string) error {
	return c.do(http.MethodPost, "/api/v3/command", map[string]string{"name": command}, nil, http.StatusCreated)
}

func (a arr) searchBatch(c conn, list string, cursor, n int) (int, string, error) {
	// Wanted lists can't be sorted by ID, so this reads the whole list.
	// pageSize has no upper limit.
	var page struct{ Records []struct{ ID int } }
	if err := c.do(http.MethodGet, "/api/v3/wanted/"+list+"?monitored=true&pageSize=1000000", nil, &page, http.StatusOK); err != nil {
		return 0, "", err
	}
	ids := make([]int, len(page.Records))
	for i, r := range page.Records {
		ids[i] = r.ID
	}
	ids = next(ids, func(id int) int { return id }, cursor, n)
	if len(ids) == 0 {
		return cursor, "nothing to search", nil
	}
	if err := c.do(http.MethodPost, "/api/v3/command", map[string]any{"name": a.command, a.idsField: ids}, nil, http.StatusCreated); err != nil {
		return 0, "", err
	}
	return ids[len(ids)-1], fmt.Sprintf("queued search for %d %s", len(ids), a.items), nil
}

// bazarr is Bazarr, whose API is unversioned.
type bazarr struct{}

func (bazarr) status(c conn) (string, string, error) {
	var s struct {
		Data struct {
			Version string `json:"bazarr_version"`
		}
	}
	err := c.do(http.MethodGet, "/api/system/status", nil, &s, http.StatusOK)
	// The status has no app name, and only Bazarr reports bazarr_version.
	if s.Data.Version == "" {
		return "", "", err
	}
	return "Bazarr", s.Data.Version, err
}

// Bazarr answers a started task with 204, and with 500 if there's no such
// task, which is the case while Bazarr isn't connected to Sonarr or Radarr.
func (bazarr) searchAll(c conn, task string) error {
	err := c.do(http.MethodPost, "/api/system/tasks?taskid="+task, nil, nil, http.StatusNoContent)
	if code, ok := errors.AsType[statusError](err); ok && code == http.StatusInternalServerError {
		return fmt.Errorf("%w; Bazarr has no task %s unless it's connected to Sonarr/Radarr", err, task)
	}
	return err
}

// searchBatch queues a search for every missing subtitle language of the batch.
func (bazarr) searchBatch(c conn, list string, cursor, n int) (int, string, error) {
	type item struct {
		SeriesID  int `json:"sonarrSeriesId"`
		EpisodeID int `json:"sonarrEpisodeId"`
		MovieID   int `json:"radarrId"`
		Missing   []struct {
			Code2      string
			Forced, HI bool
		} `json:"missing_subtitles"`
	}
	id := func(it item) int { return cmp.Or(it.EpisodeID, it.MovieID) } // only one is set
	// Without paging parameters, Bazarr returns the whole list.
	var page struct{ Data []item }
	if err := c.do(http.MethodGet, "/api/"+list+"/wanted", nil, &page, http.StatusOK); err != nil {
		return 0, "", err
	}
	items := next(page.Data, id, cursor, n)
	if len(items) == 0 {
		return cursor, "nothing to search", nil
	}
	searches := 0
	for _, it := range items {
		for _, l := range it.Missing {
			q := url.Values{"language": {l.Code2}, "forced": {strconv.FormatBool(l.Forced)}, "hi": {strconv.FormatBool(l.HI)}}
			if it.MovieID != 0 {
				q.Set("radarrid", strconv.Itoa(it.MovieID))
			} else {
				q.Set("seriesid", strconv.Itoa(it.SeriesID))
				q.Set("episodeid", strconv.Itoa(it.EpisodeID))
			}
			// Bazarr queues the search and answers 204.
			if err := c.do(http.MethodPatch, "/api/"+list+"/subtitles?"+q.Encode(), nil, nil, http.StatusNoContent); err != nil {
				return 0, "", err
			}
			searches++
		}
	}
	return id(items[len(items)-1]), fmt.Sprintf("queued %d subtitles for %d %s", searches, len(items), list), nil
}

// search is one paced search of a Wanted list, set by <setting>_PACE and
// <setting>_BATCH_SIZE. The app's searches share <APP>_URL and <APP>_API_KEY,
// where APP is setting up to the first "_".
type search struct {
	app     app
	setting string // e.g. SONARR_MISSING
	list    string // Wanted list for searchBatch
	command string // whole-list search for searchAll
	desc    string // for logs
}

// appVar is the app's settings prefix, e.g. SONARR.
func (s search) appVar() string {
	prefix, _, _ := strings.Cut(s.setting, "_")
	return prefix
}

// String is the log prefix, e.g. "sonarr: searching missing episodes".
func (s search) String() string { return strings.ToLower(s.appVar()) + ": " + s.desc }

var (
	sonarr = arr{"episodes", "EpisodeSearch", "episodeIds"}
	radarr = arr{"movies", "MoviesSearch", "movieIds"}
)

var searches = []search{
	{sonarr, "SONARR_MISSING", "missing", "MissingEpisodeSearch", "searching missing episodes"},
	{sonarr, "SONARR_UNMET", "cutoff", "CutoffUnmetEpisodeSearch", "searching cutoff unmet episodes"},
	{radarr, "RADARR_MISSING", "missing", "MissingMoviesSearch", "searching missing movies"},
	{radarr, "RADARR_UNMET", "cutoff", "CutoffUnmetMoviesSearch", "searching cutoff unmet movies"},
	{bazarr{}, "BAZARR_SERIES", "episodes", "wanted_search_missing_subtitles_series", "searching missing episode subtitles"},
	{bazarr{}, "BAZARR_MOVIES", "movies", "wanted_search_missing_subtitles_movies", "searching missing movie subtitles"},
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

// schedule reads s's pace and batch size. A zero pace means s is disabled.
func (s search) schedule() (pace time.Duration, batch int, err error) {
	raw := os.Getenv(s.setting + "_PACE")
	if raw == "" {
		return 0, 0, nil
	}
	if pace, err = parsePace(raw); err != nil {
		return 0, 0, fmt.Errorf("%s_PACE: %w", s.setting, err)
	}
	if b := os.Getenv(s.setting + "_BATCH_SIZE"); b != "" {
		if batch, err = strconv.Atoi(b); err != nil || batch <= 0 {
			return 0, 0, fmt.Errorf("%s_BATCH_SIZE: invalid %q, want a positive number", s.setting, b)
		}
	}
	// Searching a whole Wanted list more than hourly, or a batch more often
	// than every 10 minutes, hammers the indexers. This also catches configs
	// from v0.1.1 and earlier, where m meant months.
	floor, floorRaw := time.Hour, "1h"
	if batch > 0 {
		floor, floorRaw = 10*time.Minute, "10m"
	}
	if pace < floor {
		return 0, 0, fmt.Errorf("%s_PACE: %s is too often, the minimum is %s (months are M, not m)", s.setting, raw, floorRaw)
	}
	return pace, batch, nil
}

// trigger queues s's search over the whole Wanted list or, when batch > 0,
// over the next batch items, keeping the batch position in cursorPath. It
// returns a log message. A failed cursor write is only logged.
func (s search) trigger(c conn, batch int, cursorPath string) (string, error) {
	if batch == 0 {
		return "queued", s.app.searchAll(c, s.command)
	}
	b, _ := os.ReadFile(cursorPath)
	cursor, _ := strconv.Atoi(strings.TrimSpace(string(b))) // 0 if missing
	last, msg, err := s.app.searchBatch(c, s.list, cursor, batch)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(cursorPath, []byte(strconv.Itoa(last)), 0o644); err != nil {
		log.Printf("%s - cannot persist batch position: %v", s, err)
	}
	return msg, nil
}

// connect waits until the app answers as s's app and accepts the key. A
// rejected key, an auth proxy or a URL pointing at another app won't fix
// itself, so those are fatal. An unreachable app or a proxy's 5xx may just be
// starting up (or be a mistyped address, which looks the same), so those are retried.
func connect(s search, c conn) {
	prefix := s.appVar()
	for {
		name, version, err := s.app.status(c)
		if err == nil && strings.EqualFold(name, prefix) {
			log.Printf("%s - connected to %s %s at %s", s, name, version, c.url)
			return
		}
		if err == nil {
			log.Fatalf("%s - %s answers as %q, not %s; check %s_URL", s, c.url, name, strings.ToLower(prefix), prefix)
		}
		var netErr *url.Error
		var code statusError
		if !errors.As(err, &netErr) && !(errors.As(err, &code) && code >= 500) {
			log.Fatalf("%s - %s rejected the request, check %s_URL and %s_API_KEY: %v", s, c.url, prefix, prefix, err)
		}
		log.Printf("%s - cannot reach %s, retrying in %s: %v", s, c.url, retryDelay, err)
		time.Sleep(retryDelay)
	}
}

func main() {
	stateDir := os.Getenv("STATE_DIR")
	if stateDir == "" {
		stateDir = "/data"
	}
	client := &http.Client{
		// Reading a big library's whole Wanted list can take a while.
		Timeout: 2 * time.Minute,
		// Never follow redirects: Go would forward X-Api-Key to the target.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// In v0.1.2 and earlier, one batch size covered both searches of the app.
	// Ignoring it would silently turn batches into whole-list searches.
	for _, v := range []string{"SONARR_BATCH_SIZE", "RADARR_BATCH_SIZE"} {
		if os.Getenv(v) != "" {
			app, _, _ := strings.Cut(v, "_")
			log.Fatalf("%s was replaced by %s_MISSING_BATCH_SIZE and %s_UNMET_BATCH_SIZE", v, app, app)
		}
	}

	var runs []func()
	for _, s := range searches {
		pace, batch, err := s.schedule()
		if err != nil {
			log.Fatalf("%s - %v", s, err)
		}
		if pace == 0 {
			log.Printf("%s - disabled (%s_PACE unset)", s, s.setting)
			continue
		}
		prefix := s.appVar()
		c := conn{client, strings.TrimRight(os.Getenv(prefix+"_URL"), "/"), os.Getenv(prefix + "_API_KEY")}
		if c.url == "" || c.key == "" {
			log.Fatalf("%s - %s_PACE is set but %s_URL or %s_API_KEY is missing", s, s.setting, prefix, prefix)
		}
		// SONARR_MISSING -> sonarr-missing
		state := strings.ToLower(strings.ReplaceAll(s.setting, "_", "-"))
		runs = append(runs, func() { run(s, c, pace, batch, filepath.Join(stateDir, state)) })
	}
	if len(runs) == 0 {
		log.Fatal("no *_PACE variable set, nothing to do")
	}
	// Without a writable state dir, every restart re-runs every search and
	// batches never get past the first one.
	f, err := os.CreateTemp(stateDir, ".write-test-*")
	if err != nil {
		log.Fatalf("cannot write to %s as user %d: %v", stateDir, os.Getuid(), err)
	}
	f.Close()
	os.Remove(f.Name())
	for _, r := range runs {
		go r()
	}
	select {}
}

// run checks the app is reachable, then triggers the search every pace. The
// last successful run time is persisted so container restarts neither re-run
// early nor skip long (e.g. monthly) paces.
func run(s search, c conn, pace time.Duration, batch int, statePath string) {
	connect(s, c)
	var last time.Time
	if b, err := os.ReadFile(statePath); err == nil {
		last, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	}
	for {
		next := last.Add(pace)
		if wait := time.Until(next); wait > 0 {
			log.Printf("%s - next run at %s", s, next.Format(time.RFC3339))
			time.Sleep(wait)
		}
		msg, err := s.trigger(c, batch, statePath+".cursor")
		if err != nil {
			log.Printf("%s - failed, retrying in %s: %v", s, retryDelay, err)
			time.Sleep(retryDelay)
			continue
		}
		log.Printf("%s - %s", s, msg)
		last = time.Now()
		if err := os.WriteFile(statePath, []byte(last.Format(time.RFC3339)), 0o644); err != nil {
			log.Printf("%s - cannot persist last run: %v", s, err)
		}
	}
}
