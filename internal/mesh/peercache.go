package mesh

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The peer cache keeps the public nodes this node has met, per network, so
// it can rejoin through any of them after a restart even if its configured
// seeds are gone (like a BitTorrent client's saved peers).

const maxCachedPeers = 64

func (m *Mesh) peerCachePath() string {
	if m.dataDir == "" {
		return ""
	}
	h := sha256.Sum256([]byte(m.key))
	return filepath.Join(m.dataDir, "peers-"+hex.EncodeToString(h[:6])+".json")
}

// loadPeerCache adds saved public nodes to the seeds.
func (m *Mesh) loadPeerCache() {
	p := m.peerCachePath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var urls []string
	if json.Unmarshal(b, &urls) != nil {
		return
	}
	have := map[string]bool{m.publicURL: true}
	for _, s := range m.seeds {
		have[s] = true
	}
	for _, u := range urls {
		if !have[u] && validPeerURL(u) {
			have[u] = true
			m.seeds = append(m.seeds, u)
		}
	}
}

// savePeerCache records public nodes that just answered our hello (only
// nodes we reached ourselves, never second-hand addresses), newest first.
func (m *Mesh) savePeerCache(answered []string) {
	p := m.peerCachePath()
	if p == "" || len(answered) == 0 {
		return
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	var old []string
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &old)
	}
	seen := map[string]bool{m.publicURL: true}
	var urls []string
	for _, u := range append(append([]string{}, answered...), old...) {
		if !seen[u] && validPeerURL(u) && len(urls) < maxCachedPeers {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	b, _ := json.Marshal(urls)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, p)
	}
}

func validPeerURL(u string) bool {
	return len(u) < 300 && (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://"))
}
