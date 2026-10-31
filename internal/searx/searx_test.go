package searx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"hivecrawl/internal/types"
)

// Asking only for engines SearXNG lacks must not run its whole category.
func TestUnknownEnginesDoNotBecomeAGeneralSearch(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config" {
			json.NewEncoder(w).Encode(map[string]any{"engines": []map[string]any{{"name": "brave"}, {"name": "brave warp"}}})
			return
		}
		asked = append(asked, r.URL.Query().Get("engines"))
		w.Write([]byte(`{"results":[{"url":"https://a.example/","title":"a"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())

	out, err := c.Search(context.Background(), types.SearchRequest{Query: "q", Engines: []string{"yandex warp"}})
	if err != nil || len(out.Results) != 0 || len(asked) != 0 {
		t.Fatalf("unknown engines: results=%d err=%v asked=%v", len(out.Results), err, asked)
	}
	out, _ = c.Search(context.Background(), types.SearchRequest{Query: "q", Engines: []string{"yandex warp", "brave warp"}})
	if len(out.Results) != 1 || len(asked) != 1 || asked[0] != "brave warp" {
		t.Fatalf("mixed engines: results=%d asked=%v", len(out.Results), asked)
	}
}

func TestUnlist(t *testing.T) {
	for in, want := range map[string]string{
		`['A novel by Dickens.', 'Second']`: "A novel by Dickens.",
		`['It\'s good.']`:                   "It's good.",
		`plain text`:                        "plain text",
	} {
		if got := unlist(in); got != want {
			t.Errorf("unlist(%q) = %q, want %q", in, got, want)
		}
	}
}
