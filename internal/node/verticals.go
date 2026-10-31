package node

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"

	"hivecrawl/internal/cache"
	"hivecrawl/internal/types"
)

// Topics are the search categories that run specialist engines instead of
// the general web ones (Firecrawl's request field "categories"). Each list
// names SearXNG engines that are kept but disabled in settings.yml, so they
// never mix into ordinary web results and only run when asked for by name.
//
// Every engine here answered test queries from public nodes without an API
// key; ones that need keys or block servers are left out.
var Topics = map[string][]string{
	// Firecrawl's categories.
	"research": {"arxiv", "pubmed", "europepmc", "crossref", "openalex", "openairepublications", "openairedatasets", "pdbe"},
	"developer": {
		"github", "gitlab", "codeberg", "gitea.com", "sourcehut",
		"stackoverflow", "superuser", "askubuntu", "mdn", "microsoft learn",
		"hackernews", "discuss.python", "caddy.community", "pi-hole.community",
		"mankier", "hoogle", "gentoo", "nixos wiki", "national vulnerability database", "huggingface",
	},
	"github": {"github", "gitlab", "codeberg", "gitea.com", "sourcehut"}, // Firecrawl's older name

	// HiveCrawl's own.
	"packages": {
		"npm", "pkg.go.dev", "crates.io", "lib.rs", "hex", "packagist", "pub.dev", "docker hub",
		"alpine linux packages", "voidlinux", "cachy os packages", "huggingface", "huggingface datasets", "huggingface spaces",
	},
	"videos": {
		"youtube", "vimeo", "dailymotion", "sepiasearch", "odysee", "media.ccc.de", "bilibili",
		"acfun", "iqiyi", "360search videos", "sogou videos", "wikicommons.videos", "pixabay videos",
	},
	"social":    {"lemmy posts", "lemmy comments", "lemmy communities", "lemmy users", "mastodon users", "mastodon hashtags", "hackernews", "boardreader"},
	"books":     {"openlibrary", "internet archive", "gutenberg", "wikibooks", "wikisource", "wikiquote"},
	"music":     {"genius", "deezer", "soundcloud", "mixcloud", "radio browser", "wikicommons.audio", "fyyd", "apple podcasts"},
	"movies":    {"imdb", "rottentomatoes"},
	"apps":      {"apple app store", "google play apps", "fdroid", "steam"},
	"maps":      {"photon"},
	"reference": {"wiktionary", "wikivoyage", "wikiversity", "wikispecies"},
}

const (
	// TopicPDF is Firecrawl's "pdf" category: a web search kept to PDF files.
	TopicPDF = "pdf"
	// TopicWeb is the ordinary web search, so it can be mixed with others.
	TopicWeb = "web"
	// TopicAll asks for the web and every specialist category at once.
	TopicAll = "all"
	// TopicNews is the news search, which smart search mixes into web
	// results for time-sensitive queries (callers ask for it as a source).
	TopicNews = "news"
)

// IsTopic reports whether name is a category Topic can run.
func IsTopic(name string) bool {
	_, ok := Topics[name]
	return ok || name == TopicPDF || name == TopicWeb
}

// AllTopics is what TopicAll expands to: the web and every category,
// leaving out github and pdf, which repeat developer and web.
func AllTopics() []string {
	out := []string{TopicWeb}
	for _, n := range TopicNames() {
		if _, ok := Topics[n]; ok && n != "github" {
			out = append(out, n)
		}
	}
	return out
}

// TopicNames lists the categories, sorted, for error messages and docs.
func TopicNames() []string {
	names := []string{TopicAll, TopicPDF, TopicWeb}
	for k := range Topics {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// Topic searches one category. Specialist engines run on this node only:
// they are mostly public APIs that rarely block, and older peers would
// answer an engine list they lack with a general search. "pdf" is a full
// web search (mesh included) narrowed to PDF links.
func (n *Node) Topic(ctx context.Context, req types.SearchRequest, topic string) types.SearchResponse {
	start := time.Now()
	req.Normalize()
	if topic == TopicWeb {
		return n.Search(ctx, req)
	}
	if topic == TopicNews {
		req.Category = TopicNews
		return n.Search(ctx, req)
	}
	if topic == TopicPDF {
		limit := req.Limit
		req.Query += " filetype:pdf"
		req.Limit = min(limit*3, 100) // non-PDF hits are dropped below
		resp := n.Search(ctx, req)
		var pdfs []types.Result
		for _, r := range resp.Data.Web {
			if isPDF(r.URL) {
				pdfs = append(pdfs, r)
			}
		}
		if !resp.Success {
			return resp
		}
		return respond(pdfs, limit, resp.Meta, start)
	}
	req.Category = ""
	req.Engines = Topics[topic]
	resp, err := n.Local(ctx, req)
	if err != nil {
		return types.SearchResponse{Success: false, Error: "search failed: " + err.Error()}
	}
	return resp
}

func isPDF(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.HasSuffix(strings.ToLower(u.Path), ".pdf")
}

// EngineGroups lists this node's engines by what they search, for the
// status page: web, images and news run on ordinary searches; the rest are
// the categories, and "warp" holds the WARP copies when WARP is on. Only
// engines SearXNG actually has are listed.
func (n *Node) EngineGroups(ctx context.Context) map[string][]string {
	if n.searx == nil {
		return nil
	}
	cat := n.searx.Catalog(ctx)
	if cat == nil {
		return nil
	}
	have := map[string]bool{}
	out := map[string][]string{}
	for _, e := range cat {
		have[e.Name] = true
		if strings.HasSuffix(e.Name, warpSuffix) {
			if n.Warp {
				out["warp"] = append(out["warp"], e.Name)
			}
			continue
		}
		if !e.Enabled {
			continue
		}
		for _, g := range []string{"images", "news", "general"} {
			if slices.Contains(e.Categories, g) {
				if g == "general" {
					g = TopicWeb
				}
				out[g] = append(out[g], e.Name)
				break
			}
		}
	}
	for t, engines := range Topics {
		if t == "github" {
			continue // part of developer
		}
		for _, e := range engines {
			if have[e] {
				out[t] = append(out[t], e)
			}
		}
	}
	return out
}

// EngineCount is how many distinct engines this node can run (WARP copies
// not counted), or 0 if SearXNG has not said yet.
func (n *Node) EngineCount(ctx context.Context) int {
	seen := map[string]bool{}
	for g, engines := range n.EngineGroups(ctx) {
		if g == "warp" {
			continue
		}
		for _, e := range engines {
			seen[e] = true
		}
	}
	return len(seen)
}

// insight is a search's insights and when they were found.
type insight struct {
	v  *types.Insights
	at time.Time
}

const (
	insightTTL = time.Hour
	maxInsight = 2000
)

func (n *Node) keepInsights(key string, in *types.Insights) {
	if in == nil {
		return
	}
	n.insMu.Lock()
	defer n.insMu.Unlock()
	if len(n.insights) >= maxInsight {
		for k, e := range n.insights {
			if time.Since(e.at) > insightTTL || len(n.insights) >= maxInsight {
				delete(n.insights, k)
			}
		}
	}
	n.insights[key] = insight{in, time.Now()}
}

// Insights returns what this node's own engines found besides results for
// a public web search that has already run (nil if nothing).
func (n *Node) Insights(req types.SearchRequest) *types.Insights {
	req.Normalize()
	req.Engines = nil
	n.insMu.Lock()
	defer n.insMu.Unlock()
	e, ok := n.insights[cache.Key(req)]
	if !ok || time.Since(e.at) > insightTTL {
		return nil
	}
	return e.v
}
