// getterr periodically triggers Sonarr/Radarr "Wanted" searches
// (missing + cutoff unmet) via their /api/v3/command endpoint.
package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const retryDelay = 5 * time.Minute

type job struct {
	app, command, paceVar string
}

var jobs = []job{
	{"sonarr", "MissingEpisodeSearch", "SONARR_MISSING_PACE"},
	{"sonarr", "CutoffUnmetEpisodeSearch", "SONARR_UNMET_PACE"},
	{"radarr", "MissingMoviesSearch", "RADARR_MISSING_PACE"},
	{"radarr", "CutoffUnmetMoviesSearch", "RADARR_UNMET_PACE"},
}

var units = map[byte]time.Duration{
	'h': time.Hour,
	'd': 24 * time.Hour,
	'w': 7 * 24 * time.Hour,
	'm': 30 * 24 * time.Hour,
}

// parsePace parses "<N><unit>" with unit h (hours), d (days), w (weeks), m (30-day months).
func parsePace(s string) (time.Duration, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid pace %q", s)
	}
	unit, ok := units[s[len(s)-1]]
	n, err := strconv.Atoi(s[:len(s)-1])
	if !ok || err != nil || n <= 0 || time.Duration(n) > math.MaxInt64/unit {
		return 0, fmt.Errorf("invalid pace %q: want <N>h, <N>d, <N>w or <N>m", s)
	}
	return time.Duration(n) * unit, nil
}

func trigger(client *http.Client, baseURL, apiKey, command string) error {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v3/command", strings.NewReader(`{"name":"`+command+`"}`))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	// Sonarr/Radarr answer a queued command with 201; anything else (e.g. an
	// auth proxy's redirect or login page) means nothing was queued.
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return nil
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
			log.Printf("%s %s: disabled (%s unset)", j.app, j.command, j.paceVar)
			continue
		}
		pace, err := parsePace(raw)
		if err != nil {
			log.Fatalf("%s: %v", j.paceVar, err)
		}
		prefix := strings.ToUpper(j.app)
		baseURL := strings.TrimRight(os.Getenv(prefix+"_URL"), "/")
		apiKey := os.Getenv(prefix + "_API_KEY")
		if baseURL == "" || apiKey == "" {
			log.Fatalf("%s is set but %s_URL or %s_API_KEY is missing", j.paceVar, prefix, prefix)
		}
		go run(client, j, pace, baseURL, apiKey, filepath.Join(stateDir, j.command))
		started++
	}
	if started == 0 {
		log.Fatal("no *_PACE variable set, nothing to do")
	}
	select {}
}

// run triggers the job every pace. The last successful run time is persisted
// so container restarts neither re-run early nor skip long (e.g. monthly) paces.
func run(client *http.Client, j job, pace time.Duration, baseURL, apiKey, statePath string) {
	var last time.Time
	if b, err := os.ReadFile(statePath); err == nil {
		last, _ = time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	}
	for {
		next := last.Add(pace)
		if wait := time.Until(next); wait > 0 {
			log.Printf("%s %s: next run at %s", j.app, j.command, next.Format(time.RFC3339))
			time.Sleep(wait)
		}
		if err := trigger(client, baseURL, apiKey, j.command); err != nil {
			log.Printf("%s %s: failed, retrying in %s: %v", j.app, j.command, retryDelay, err)
			time.Sleep(retryDelay)
			continue
		}
		log.Printf("%s %s: queued", j.app, j.command)
		last = time.Now()
		if err := os.WriteFile(statePath, []byte(last.Format(time.RFC3339)), 0o644); err != nil {
			log.Printf("%s %s: cannot persist last run: %v", j.app, j.command, err)
		}
	}
}
