// Package update asks GitHub for the newest HiveCrawl release so the node
// can tell its owner when an update is out. It only reads public release
// data; installing an update is done on the host by update.sh, never by
// the node itself.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ReleasesURL is the GitHub API endpoint for the newest release.
const ReleasesURL = "https://api.github.com/repos/savor3/hivecrawl/releases/latest"

// Status is what the node reports about updates.
type Status struct {
	Current   string    `json:"current"`
	Latest    string    `json:"latest,omitempty"`
	Available bool      `json:"available"`
	URL       string    `json:"url,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
	// Updater is the updater service's report; nil when it isn't installed.
	Updater *Updater `json:"updater,omitempty"`
}

// Checker polls the releases endpoint every Interval.
type Checker struct {
	Current  string
	URL      string // defaults to ReleasesURL
	Interval time.Duration
	Client   *http.Client

	mu     sync.Mutex
	status Status
	told   string // newest version already logged
}

// Status returns the result of the last check.
func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	s.Current = c.Current
	return s
}

// Start checks shortly after boot, then every Interval, until ctx ends.
func (c *Checker) Start(ctx context.Context) {
	if c.Interval <= 0 {
		c.Interval = 12 * time.Hour
	}
	go func() {
		t := time.NewTimer(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.Check(ctx)
				t.Reset(c.Interval)
			}
		}
	}()
}

// Check fetches the newest release once and records the result.
func (c *Checker) Check(ctx context.Context) Status {
	url := c.URL
	if url == "" {
		url = ReleasesURL
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	s := Status{CheckedAt: time.Now()}
	tag, page, err := latest(ctx, client, url)
	if err != nil {
		s.Error = err.Error()
	} else {
		s.Latest, s.URL = strings.TrimPrefix(tag, "v"), page
		s.Available = Newer(s.Latest, c.Current)
	}
	c.mu.Lock()
	c.status = s
	tell := s.Available && c.told != s.Latest
	if tell {
		c.told = s.Latest
	}
	c.mu.Unlock()
	if tell {
		log.Printf("HiveCrawl %s is out (this node runs %s): %s — update with ./update.sh", s.Latest, c.Current, s.URL)
	}
	return c.Status()
}

func latest(ctx context.Context, client *http.Client, url string) (tag, page string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hivecrawl-update-check")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", "", fmt.Errorf("no releases published yet")
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("release check: HTTP %d", resp.StatusCode)
	}
	var r struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", err
	}
	if r.TagName == "" {
		return "", "", fmt.Errorf("release check: no tag in reply")
	}
	return r.TagName, r.HTMLURL, nil
}

// Newer reports whether version a is higher than b. Both are dotted numbers
// with an optional leading "v"; anything after "-" or "+" is ignored, and a
// version that does not parse (a dev build) is never newer and always older.
func Newer(a, b string) bool {
	pa, okA := parse(a)
	pb, okB := parse(b)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// Updater is what the updater service (deploy/updater) reports in the folder
// it shares with the node.
type Updater struct {
	Running   bool   `json:"running"` // it wrote its status in the last 3 minutes
	Auto      bool   `json:"auto"`
	Available bool   `json:"available"` // a newer release than the running one is out
	State     string `json:"state"`     // starting, checking, updating, idle or failed
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	CheckedAt string `json:"checkedAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
	Message   string `json:"message,omitempty"`
	Pending   bool   `json:"pending,omitempty"` // a check was asked for and has not started yet
}

// ReadUpdater returns the updater's status from dir, or nil when there is
// no updater (no status file).
func ReadUpdater(dir string) *Updater {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		return nil
	}
	var u struct {
		Updater
		SeenAt string `json:"seenAt"`
	}
	if json.Unmarshal(b, &u) != nil {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, u.SeenAt); err == nil {
		u.Updater.Running = time.Since(t) < 3*time.Minute
	}
	if _, err := os.Stat(filepath.Join(dir, "check-now")); err == nil {
		u.Updater.Pending = true
	}
	return &u.Updater
}

// SetAuto turns automatic installs on or off; the updater applies it within
// a few seconds and keeps it across restarts.
func SetAuto(dir string, on bool) error {
	if dir == "" {
		return fmt.Errorf("no updater is set up for this node")
	}
	v := "false"
	if on {
		v = "true"
	}
	if err := os.WriteFile(filepath.Join(dir, "auto-update"), []byte(v), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "auto-changed"), nil, 0o644)
}

// RequestCheck asks the updater to check (and, with auto-update on, install)
// now. The updater picks it up within a few seconds.
func RequestCheck(dir string) error {
	if dir == "" {
		return fmt.Errorf("no updater is set up for this node")
	}
	return os.WriteFile(filepath.Join(dir, "check-now"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}
