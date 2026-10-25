package mesh

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Every mesh request is signed twice:
//   - with the node's own ed25519 key: proves which node sent it. A node's ID
//     is derived from its public key, so an ID cannot be claimed by anyone
//     else (no stealing another node's priority or reputation);
//   - with an HMAC of the network key: proves membership of this network.
//
// The timestamp limits replays to a short window.
const (
	hdrNode = "X-HC-Node" // node ID, derived from the public key
	hdrPub  = "X-HC-Pub"  // base64 ed25519 public key
	hdrTS   = "X-HC-TS"
	hdrSig  = "X-HC-Sig" // base64 ed25519 signature
	hdrNet  = "X-HC-Net" // hex HMAC-SHA256 with the network key
	maxSkew = 2 * time.Minute
)

// Identity is a node's long-lived key pair.
type Identity struct {
	ID   string
	Pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDFor derives a node ID from a public key: "hc" + 16 base32 chars.
func IDFor(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return "hc" + strings.ToLower(idEncoding.EncodeToString(h[:10]))
}

// NewIdentity creates a fresh key pair (tests, or before persisting).
func NewIdentity() *Identity {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return &Identity{ID: IDFor(pub), Pub: pub, priv: priv}
}

// LoadIdentity reads the key from dir/node.key, creating it on first run.
func LoadIdentity(dir string) (*Identity, error) {
	path := filepath.Join(dir, "node.key")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		id := NewIdentity()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(id.priv.Seed())+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("save node key: %w", err)
		}
		return id, nil
	}
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a valid node key", path)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Identity{ID: IDFor(pub), Pub: pub, priv: priv}, nil
}

func message(node, method, path, ts string, body []byte) []byte {
	bh := sha256.Sum256(body)
	return []byte("hivecrawl/1\n" + node + "\n" + method + "\n" + path + "\n" + ts + "\n" + hex.EncodeToString(bh[:]))
}

func netMAC(key string, msg []byte) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

func signHeaders(h http.Header, id *Identity, key, method, path string, body []byte) {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := message(id.ID, method, path, ts, body)
	h.Set(hdrNode, id.ID)
	h.Set(hdrPub, base64.StdEncoding.EncodeToString(id.Pub))
	h.Set(hdrTS, ts)
	h.Set(hdrSig, base64.StdEncoding.EncodeToString(ed25519.Sign(id.priv, msg)))
	h.Set(hdrNet, netMAC(key, msg))
}

var errAuth = errors.New("mesh auth failed")

// verify checks a signed request and returns the sender's node ID.
func verify(key string, r *http.Request, body []byte) (string, error) {
	if key == "" {
		return "", errAuth
	}
	node, ts := r.Header.Get(hdrNode), r.Header.Get(hdrTS)
	pub, err1 := base64.StdEncoding.DecodeString(r.Header.Get(hdrPub))
	sig, err2 := base64.StdEncoding.DecodeString(r.Header.Get(hdrSig))
	if node == "" || ts == "" || err1 != nil || err2 != nil || len(pub) != ed25519.PublicKeySize {
		return "", errAuth
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errAuth
	}
	if d := time.Since(time.Unix(sec, 0)); d > maxSkew || d < -maxSkew {
		return "", errAuth
	}
	if IDFor(pub) != node {
		return "", errAuth // ID does not belong to this key
	}
	msg := message(node, r.Method, r.URL.Path, ts, body)
	if !hmac.Equal([]byte(netMAC(key, msg)), []byte(r.Header.Get(hdrNet))) {
		return "", errAuth
	}
	if !ed25519.Verify(pub, msg, sig) {
		return "", errAuth
	}
	return node, nil
}
