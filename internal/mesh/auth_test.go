package mesh

import (
	"encoding/base64"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

const testKey = "0123456789abcdef-test"

func TestSignVerify(t *testing.T) {
	id := NewIdentity()
	body := []byte(`{"query":"x"}`)
	r := httptest.NewRequest("POST", "/mesh/v1/search", nil)
	signHeaders(r.Header, id, testKey, "POST", "/mesh/v1/search", body)
	if got, err := verify(testKey, r, body); err != nil || got != id.ID {
		t.Fatalf("valid request rejected: %v %q", err, got)
	}
	if _, err := verify("another-key-123456", r, body); err == nil {
		t.Fatal("wrong network key accepted")
	}
	if _, err := verify(testKey, r, []byte(`{"query":"y"}`)); err == nil {
		t.Fatal("tampered body accepted")
	}
	r2 := httptest.NewRequest("POST", "/mesh/v1/hello", nil)
	r2.Header = r.Header.Clone()
	if _, err := verify(testKey, r2, body); err == nil {
		t.Fatal("signature replayed on another path accepted")
	}
}

// A node that knows the network key still cannot act as another node.
func TestCannotImpersonateAnotherNode(t *testing.T) {
	victim, attacker := NewIdentity(), NewIdentity()
	body := []byte(`{}`)

	// Attacker signs with its own key but claims the victim's ID.
	r := httptest.NewRequest("POST", "/mesh/v1/search", nil)
	signHeaders(r.Header, attacker, testKey, "POST", "/mesh/v1/search", body)
	r.Header.Set(hdrNode, victim.ID)
	if _, err := verify(testKey, r, body); err == nil {
		t.Fatal("request claiming another node's ID accepted")
	}

	// Attacker also swaps in the victim's public key: signature no longer verifies.
	r.Header.Set(hdrPub, base64.StdEncoding.EncodeToString(victim.Pub))
	if _, err := verify(testKey, r, body); err == nil {
		t.Fatal("forged signature accepted")
	}
}

func TestVerifyRejectsOldTimestamp(t *testing.T) {
	id := NewIdentity()
	r := httptest.NewRequest("POST", "/p", nil)
	signHeaders(r.Header, id, testKey, "POST", "/p", nil)
	r.Header.Set(hdrTS, strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10))
	if _, err := verify(testKey, r, nil); err == nil {
		t.Fatal("stale request accepted")
	}
}

func TestIdentityPersists(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || len(a.ID) != 18 {
		t.Fatalf("identity not stable across restarts: %s vs %s", a.ID, b.ID)
	}
}
