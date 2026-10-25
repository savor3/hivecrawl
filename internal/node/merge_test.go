package node

import (
	"testing"
	"time"

	"hivecrawl/internal/types"
)

func TestMergeDedupesAndBoostsShared(t *testing.T) {
	a := []types.Result{{URL: "https://a.com/x", Engines: []string{"google"}}, {URL: "https://shared.com/"}}
	b := []types.Result{{URL: "https://www.shared.com", Description: "longer description", Engines: []string{"brave"}}, {URL: "https://b.com"}}
	out := merge([][]types.Result{a, b})
	if len(out) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(out), out)
	}
	if out[0].URL != "https://shared.com/" {
		t.Fatalf("result found in both lists should rank first, got %s", out[0].URL)
	}
	if out[0].Description != "longer description" || len(out[0].Engines) != 1 || out[0].Engines[0] != "brave" {
		t.Fatalf("merge fields wrong: %+v", out[0])
	}
	for i, r := range out {
		if r.Position != i+1 {
			t.Fatalf("positions not renumbered: %+v", out)
		}
	}
}

func TestApplyTimeRange(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	res := []types.Result{
		{URL: "https://wikipedia.org/evergreen"},                          // undated
		{URL: "https://news.example/old", Date: "2026-08-01T00:00:00Z"},   // outside a week
		{URL: "https://news.example/today", Date: "2026-10-25T09:00:00Z"}, // inside
		{URL: "https://news.example/3days", Date: "2026-10-22T09:00:00Z"}, // inside
	}
	out := applyTimeRange(res, "week", now)
	if len(out) != 3 {
		t.Fatalf("want 3 results (old one dropped), got %+v", out)
	}
	if out[0].URL != "https://news.example/today" || out[1].URL != "https://news.example/3days" || out[2].URL != "https://wikipedia.org/evergreen" {
		t.Fatalf("want dated results first, undated last: %+v", out)
	}
	if out[2].Position != 3 {
		t.Fatal("positions not renumbered")
	}
	if got := applyTimeRange(res, "", now); len(got) != 4 {
		t.Fatal("no time range must not filter")
	}
}
