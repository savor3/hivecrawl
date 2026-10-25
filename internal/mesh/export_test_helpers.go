package mesh

// TestAddPeer adds a directly reachable peer (for tests in other packages).
func (m *Mesh) TestAddPeer(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.peer(id); p != nil {
		p.URL = "http://" + id + ".test"
		seen(p)
	}
}
