package mesh

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"hivecrawl/internal/types"
)

// The probe store keeps this node's last good answer to each probe query.
// A node whose engines are blocked right now (CAPTCHAs, rate limits, WARP
// timing out) is exactly the node that needs the mesh, but it cannot run a
// fresh search to answer a probe. It answers from here instead: it still
// has to have produced real results itself recently, so a script that only
// takes gets nothing from this.

const (
	probeAnswerFor    = 7 * 24 * time.Hour // how old a stored answer may be
	probeRefreshEvery = 6 * time.Hour
)

type storedAnswer struct {
	URLs []string  `json:"urls"`
	At   time.Time `json:"at"`
}

type probeStore struct {
	mu   sync.Mutex
	path string
	ans  map[string]storedAnswer
}

func newProbeStore(dataDir string) *probeStore {
	s := &probeStore{ans: map[string]storedAnswer{}}
	if dataDir != "" {
		s.path = filepath.Join(dataDir, "probe-answers.json")
		if b, err := os.ReadFile(s.path); err == nil {
			json.Unmarshal(b, &s.ans)
		}
	}
	return s
}

// get returns a stored answer younger than probeAnswerFor.
func (s *probeStore) get(query string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.ans[query]
	if !ok || time.Since(a.At) > probeAnswerFor {
		return nil
	}
	return a.URLs
}

func (s *probeStore) age(query string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.ans[query]
	if !ok {
		return probeAnswerFor + time.Hour
	}
	return time.Since(a.At)
}

func (s *probeStore) put(query string, urls []string) {
	if len(urls) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ans[query] = storedAnswer{URLs: urls, At: time.Now()}
	if s.path == "" {
		return
	}
	b, _ := json.Marshal(s.ans)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, s.path)
	}
}

// probeSearch runs a probe query on this node and remembers a good answer;
// when the search finds nothing it falls back to the stored answer.
func (m *Mesh) probeSearch(ctx context.Context, query string) []string {
	var urls []string
	if resp, err := m.probeLocal(ctx, types.SearchRequest{Query: query, Limit: 10, LocalOnly: true}); err == nil {
		for _, r := range resp.Data.Web {
			urls = append(urls, r.URL)
		}
	}
	if len(urls) > 0 {
		m.probeAnswers.put(query, urls)
		return urls
	}
	return m.probeAnswers.get(query)
}

// refreshProbeAnswers keeps the store warm while this node's engines work,
// so it is ready the day they stop working. Probe queries are cached
// everywhere, so this costs a few searches every few hours.
func (m *Mesh) refreshProbeAnswers(ctx context.Context) {
	for {
		for _, q := range probeQueries {
			if m.probeAnswers.age(q) < probeRefreshEvery {
				continue
			}
			qctx, cancel := context.WithTimeout(ctx, probeTimeout)
			m.probeSearch(qctx, q)
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond): // spread the searches out
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(probeRefreshEvery / 6):
		}
	}
}
