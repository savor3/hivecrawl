package node

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Smart search: an ordinary web search that clearly looks like code,
// research, a song or the like also asks that category's engines (Topics),
// and a few of their results are mixed into the web results. The guess is
// rule-based (words and code-like patterns), never a model.

// intentWords are the words and phrases that point at each category. Each
// match scores one point; a query needs a point to get a category.
var intentWords = map[string][]string{
	"developer": {
		"error", "exception", "traceback", "stack trace", "segfault", "undefined reference", "compile", "compiler",
		"regex", "api", "sdk", "framework", "function", "syntax", "deprecated", "stackoverflow", "stack overflow",
		"github", "gitlab", "git", "docker", "dockerfile", "kubernetes", "k8s", "nginx", "linux command", "bash",
		"python", "javascript", "typescript", "golang", "rust", "java", "kotlin", "swiftui", "c++", "c#", "php", "ruby",
		"sql", "postgres", "mysql", "css", "html", "react", "vue", "svelte", "node.js", "nodejs", "django", "flask",
		"fastapi", "spring boot", "async", "await", "goroutine", "mutex", "segmentation fault", "null pointer",
	},
	"research": {
		"papers", "research paper", "paper on", "white paper", "studies", "journal", "arxiv", "doi", "meta-analysis", "clinical trial",
		"pubmed", "peer reviewed", "peer-reviewed", "research on", "et al", "systematic review", "preprint",
	},
	"packages": {
		"npm", "pypi", "pip install", "crates.io", "library for", "docker image", "huggingface",
		"hugging face", "model weights", "gguf", "go module", "composer require",
	},
	"videos": {"video", "videos", "youtube", "vlog", "full episode", "livestream", "talk recording"},
	"music":  {"lyrics", "song", "songs", "album", "podcast", "podcasts", "playlist", "singer", "radio station"},
	"movies": {"movie", "movies", "film", "imdb", "box office", "cast of", "rotten tomatoes"},
	"books":  {"novel", "novels", "ebook", "e-book", "audiobook", "author of", "read online", "public domain"},
	"apps":   {"apk", "android app", "ios app", "iphone app", "app store", "play store", "f-droid", "steam game"},
	"social": {"reddit", "forum", "discussion", "opinions", "what do people think", "hacker news", "mastodon", "lemmy"},
	// Time-sensitive searches get dated news (TopicNews).
	TopicNews: {
		"latest", "today", "yesterday", "this week", "breaking", "news", "announced", "announces", "launched",
		"released", "release date", "price of", "stock price", "share price", "election", "lawsuit", "outage",
		"acquisition", "acquires", "earnings", "update on",
	},
}

// codeLike matches things only code has: calls, paths, operators,
// identifiers in camelCase or snake_case, source file names and error
// types (TypeError, NullPointerException).
var codeLike = regexp.MustCompile(`[a-z_]\w*\.[a-z_]\w*\(|\w\(\)|::|=>|->|!=|==|\b[a-z]+_[a-z_]+\b|\b[a-z]+[A-Z]\w*\b|\.(py|js|ts|go|rs|java|rb|php|cpp|yaml|yml|toml|json)\b|\b[A-Z]\w*(Error|Exception)\b`)

// maxIntents caps how many categories one search adds.
const maxIntents = 2

// Intents returns the categories a web search for q would also gain from,
// best match first. Most queries get none.
func Intents(q string) []string {
	low := " " + strings.ToLower(q) + " "
	score := map[string]int{}
	for topic, words := range intentWords {
		for _, w := range words {
			if containsWord(low, w) {
				score[topic]++
			}
		}
	}
	if codeLike.MatchString(q) {
		score["developer"] += 2
	}
	// This year or next in the query: it is about something recent.
	if y := time.Now().Year(); containsWord(low, strconv.Itoa(y)) || containsWord(low, strconv.Itoa(y+1)) {
		score[TopicNews]++
	}
	var out []string
	for t, s := range score {
		if s > 0 {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if score[out[i]] != score[out[j]] {
			return score[out[i]] > score[out[j]]
		}
		return out[i] < out[j]
	})
	if len(out) > maxIntents {
		out = out[:maxIntents]
	}
	return out
}

// containsWord finds w in padded, lower-cased text as a whole word or
// phrase (so "api" does not match "capital").
func containsWord(text, w string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], w)
		if j < 0 {
			return false
		}
		j += i
		before, after := text[j-1], byte(' ')
		if j+len(w) < len(text) {
			after = text[j+len(w)]
		}
		if !isWordByte(before) && !isWordByte(after) {
			return true
		}
		i = j + 1
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}
