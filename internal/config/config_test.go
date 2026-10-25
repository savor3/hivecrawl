package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureAPIKeyGeneratesOnceAndKeeps(t *testing.T) {
	dir := t.TempDir()
	c := &Config{DataDir: dir, Listen: ":3002", UIURL: "http://localhost:3012"}
	created, err := c.EnsureAPIKey()
	if err != nil || !created || !strings.HasPrefix(c.APIKeys[0], "hc-") || len(c.APIKeys[0]) != 35 {
		t.Fatalf("first: %v %v %v", created, err, c.APIKeys)
	}
	fi, _ := os.Stat(filepath.Join(dir, "api-key"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	c2 := &Config{DataDir: dir}
	if created, _ := c2.EnsureAPIKey(); created || c2.APIKeys[0] != c.APIKeys[0] {
		t.Fatal("key must be reused after restart")
	}
	if got := c.LoginURL(); got != "http://localhost:3012/#key="+c.APIKeys[0] {
		t.Fatalf("login url %q", got)
	}
	c3 := &Config{DataDir: t.TempDir(), APIKeys: []string{"hc-env"}}
	if created, _ := c3.EnsureAPIKey(); created || c3.APIKeys[0] != "hc-env" {
		t.Fatal("HC_API_KEYS must win")
	}
}

func TestNetworkModes(t *testing.T) {
	for _, tc := range []struct {
		env          map[string]string
		network, key string
		peers        int
		wantErr      bool
	}{
		{map[string]string{}, "community", CommunityKey, len(CommunityPeers), false},
		{map[string]string{"HC_PEERS": "https://x.example"}, "community", CommunityKey, len(CommunityPeers) + 1, false},
		{map[string]string{"HC_NETWORK_KEY": "my-own-private-key-123"}, "private", "my-own-private-key-123", 0, false},
		{map[string]string{"HC_NETWORK": "off"}, "off", "", 0, false},
		{map[string]string{"HC_NETWORK": "bogus"}, "", "", 0, true},
	} {
		for _, k := range []string{"HC_PEERS", "HC_NETWORK_KEY", "HC_NETWORK"} {
			t.Setenv(k, tc.env[k])
		}
		c, err := Load()
		if tc.wantErr {
			if err == nil {
				t.Errorf("%v: want error", tc.env)
			}
			continue
		}
		if err != nil || c.Network != tc.network || c.NetworkKey != tc.key || len(c.Peers) != tc.peers {
			t.Errorf("%v: got %q key=%q peers=%v err=%v", tc.env, c.Network, c.NetworkKey, c.Peers, err)
		}
	}
}
