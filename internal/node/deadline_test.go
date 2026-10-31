package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/searx"
	"hivecrawl/internal/types"
)

// A SearXNG that never answers (overloaded, queued) must not hold a search
// past the budget, and the answer is a retry, not a failure.
func TestSearchKeepsToBudgetWhenSearxHangs(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	n := New("t", cache.New(time.Minute, 100), searx.New(hang.URL, &http.Client{Timeout: 15 * time.Second}), 1)
	start := time.Now()
	resp := n.Search(context.Background(), types.SearchRequest{Query: "anything"})
	if took := time.Since(start); took > searchBudget+time.Second {
		t.Fatalf("search took %v, budget %v", took, searchBudget)
	}
	if !resp.Success || !resp.Meta.Retry {
		t.Fatalf("want a retry answer, got success=%v retry=%v err=%q", resp.Success, resp.Meta.Retry, resp.Error)
	}
}
