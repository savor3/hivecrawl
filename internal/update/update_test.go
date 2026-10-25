package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.4.0", "0.3.0", true},
		{"v0.3.1", "0.3.0", true},
		{"0.3.0", "0.3.0", false},
		{"0.2.9", "0.3.0", false},
		{"1.0", "0.9.9", true},
		{"0.10.0", "0.9.0", true},
		{"0.4.0-rc1", "0.3.0", true},
		{"0.4.0", "dev", true},
		{"garbage", "0.3.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v0.9.0","html_url":"https://github.com/savor3/hivecrawl/releases/tag/v0.9.0"}`))
	}))
	defer srv.Close()
	c := &Checker{Current: "0.3.0", URL: srv.URL}
	s := c.Check(context.Background())
	if !s.Available || s.Latest != "0.9.0" || s.Current != "0.3.0" || s.URL == "" {
		t.Fatalf("got %+v", s)
	}

	c.Current = "0.9.0"
	if s := c.Check(context.Background()); s.Available {
		t.Fatalf("same version reported as update: %+v", s)
	}
}

func TestCheckNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	s := (&Checker{Current: "0.3.0", URL: srv.URL}).Check(context.Background())
	if s.Available || s.Error == "" {
		t.Fatalf("got %+v", s)
	}
}

func TestUpdaterStatus(t *testing.T) {
	dir := t.TempDir()
	if ReadUpdater(dir) != nil {
		t.Fatal("no status file must mean no updater")
	}
	seen := time.Now().UTC().Format(time.RFC3339)
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"auto":true,"state":"idle","current":"v0.4.0","latest":"v0.4.0","seenAt":"`+seen+`","message":"up to date"}`), 0o644)
	u := ReadUpdater(dir)
	if u == nil || !u.Running || !u.Auto || u.State != "idle" || u.Pending {
		t.Fatalf("got %+v", u)
	}
	if err := RequestCheck(dir); err != nil {
		t.Fatal(err)
	}
	if !ReadUpdater(dir).Pending {
		t.Fatal("check-now flag not seen")
	}

	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	os.WriteFile(filepath.Join(dir, "status.json"), []byte(`{"auto":true,"state":"idle","seenAt":"`+old+`"}`), 0o644)
	if ReadUpdater(dir).Running {
		t.Fatal("a stale status must not count as running")
	}
}
