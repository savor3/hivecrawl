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

func TestInsights(t *testing.T) {
	var sr searxResponse
	body := `{"suggestions":["a","b"],"corrections":["python tutorial"],
	  "answers":[{"answer":"6.2 mi"},"plain"],
	  "infoboxes":[{"infobox":"Albert Einstein","content":"Physicist","img_src":"https://x/e.jpg","engine":"wikipedia",
	    "urls":[{"url":"https://en.wikipedia.org/wiki/Albert_Einstein"}],"attributes":[{"label":"Born","value":"1879"},{"label":"Img","value":{"src":"x"}}]}]}`
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatal(err)
	}
	in := insights(sr)
	if in == nil || len(in.Suggestions) != 2 || in.Corrections[0] != "python tutorial" || len(in.Answers) != 2 || in.Answers[0] != "6.2 mi" {
		t.Fatalf("got %+v", in)
	}
	if b := in.Infobox; b == nil || b.Title != "Albert Einstein" || b.URL == "" || len(b.Attributes) != 1 {
		t.Fatalf("infobox %+v", in.Infobox)
	}
	if insights(searxResponse{}) != nil {
		t.Fatal("empty insights should be nil")
	}
}

func TestSameScriptFirst(t *testing.T) {
	res := []types.Result{{Title: "알베르트 아인슈타인"}, {Title: "Albert Einstein"}, {Title: "123"}, {Title: "Einstein – Wikipedia"}}
	got := sameScriptFirst("albert einstein", res)
	if got[0].Title != "Albert Einstein" || got[3].Title != "알베르트 아인슈타인" || got[0].Position != 1 {
		t.Fatalf("got %v", got)
	}
	// A Korean query keeps Korean results first.
	if got := sameScriptFirst("아인슈타인", res); got[0].Title != "알베르트 아인슈타인" {
		t.Fatalf("korean query: %v", got)
	}
}
