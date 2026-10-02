package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The daemon asks GitHub now and then whether a newer release exists, so that
// the status page can say so. It never downloads or installs anything.

const releasesAPI = "https://api.github.com/repos/holdmysocks/ps5-tailscale/releases/latest"

type releaseInfo struct {
	Version string // without the leading "v"
	URL     string
}

// parseVersion reads "v1.2.3" or "1.2.3-dev" as its three numbers.
func parseVersion(s string) (v [3]int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// newerVersion reports whether latest is a later release than current.
func newerVersion(current, latest string) bool {
	c, ok1 := parseVersion(current)
	l, ok2 := parseVersion(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := range c {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func fetchLatestRelease(ctx context.Context, url string) (releaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	req.Header.Set("User-Agent", "ps5-tailscale/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return releaseInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{}, &httpStatusError{resp.Status}
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return releaseInfo{}, err
	}
	return releaseInfo{Version: strings.TrimPrefix(rel.TagName, "v"), URL: rel.HTMLURL}, nil
}

type httpStatusError struct{ status string }

func (e *httpStatusError) Error() string { return "unexpected response: " + e.status }

// watchForUpdates checks shortly after start and then twice a day, for as
// long as the setting is on.
func (d *daemon) watchForUpdates(ctx context.Context) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(12 * time.Hour)

		d.mu.Lock()
		enabled := d.cfg.CheckUpdates
		d.mu.Unlock()
		if !enabled {
			d.mu.Lock()
			d.latest = releaseInfo{}
			d.mu.Unlock()
			continue
		}
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rel, err := fetchLatestRelease(reqCtx, releasesAPI)
		cancel()
		if err != nil {
			d.logf("update check: %v", err)
			timer.Reset(time.Hour)
			continue
		}
		d.mu.Lock()
		known := d.latest.Version
		d.latest = rel
		d.mu.Unlock()
		if d.debug != nil {
			d.debug.Printf("update check: the latest release is %s", rel.Version)
		}
		if rel.Version != known && newerVersion(version, rel.Version) {
			d.logf("a newer release is available: %s (running %s)", rel.Version, version)
		}
	}
}
