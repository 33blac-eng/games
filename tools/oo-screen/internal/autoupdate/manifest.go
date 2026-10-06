// Package autoupdate implements S6: agent auto-update from an ed25519-signed
// manifest with a pinned public key, staged (percentage) rollout and automatic
// rollback when the new binary fails its health check.
//
// Flow (all of it is OFF unless the agent is started with -auto-update-url
// AND built with a pinned key):
//
//  1. Check: fetch signed manifest, verify signature with the pinned key,
//     reject older/equal versions and nodes outside the rollout bucket.
//  2. Stage: download the artifact, verify size+sha256 from the (signed)
//     manifest, write <exe>.new.
//  3. Swap: rename <exe> -> <exe>.old, <exe>.new -> <exe>, write a pending
//     marker; caller restarts the process.
//  4. Startup of the new binary (Startup): marker present -> run health
//     check; ok -> Commit (drop .old + marker); fail, or too many starts
//     without reaching healthy (crash loop) -> Rollback (restore .old).
package autoupdate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Artifact is one downloadable binary in the manifest.
type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest describes one release. It is only trusted after Verify.
type Manifest struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Channel string `json:"channel,omitempty"`
	// RolloutPercent 0..100: share of nodes (by stable hash bucket) that take
	// this release. Raising it in a newly signed manifest widens the rollout.
	RolloutPercent int        `json:"rollout_percent"`
	IssuedAt       time.Time  `json:"issued_at"`
	Artifacts      []Artifact `json:"artifacts"`
}

// Signed is the wire envelope: the exact signed bytes plus signature.
type Signed struct {
	Manifest  string `json:"manifest"`  // base64(std) of manifest JSON
	Signature string `json:"signature"` // base64(std) ed25519 over the decoded bytes
}

// Product is the only product name the agent accepts.
const Product = "oo-agent"

// Errors returned by Verify / ParsePublicKey.
var (
	ErrNoKey     = errors.New("autoupdate: no pinned public key")
	ErrBadSig    = errors.New("autoupdate: manifest signature invalid")
	ErrBadFormat = errors.New("autoupdate: malformed manifest")
)

// ParsePublicKey accepts a hex or base64 ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, ErrNoKey
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b), nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == ed25519.PublicKeySize {
		return ed25519.PublicKey(b), nil
	}
	return nil, fmt.Errorf("autoupdate: public key must be %d bytes hex/base64", ed25519.PublicKeySize)
}

// Sign produces the envelope (used by oo-update-sign and tests).
func Sign(priv ed25519.PrivateKey, m Manifest) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Signed{
		Manifest:  base64.StdEncoding.EncodeToString(raw),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, raw)),
	})
}

// Verify checks the envelope against the pinned key and returns the manifest.
// The manifest body is not parsed before the signature is checked.
func Verify(pub ed25519.PublicKey, envelope []byte) (*Manifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrNoKey
	}
	var s Signed
	if err := json.Unmarshal(envelope, &s); err != nil {
		return nil, ErrBadFormat
	}
	raw, err1 := base64.StdEncoding.DecodeString(s.Manifest)
	sig, err2 := base64.StdEncoding.DecodeString(s.Signature)
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrBadFormat
	}
	if !ed25519.Verify(pub, raw, sig) {
		return nil, ErrBadSig
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, ErrBadFormat
	}
	if m.Product != Product || m.Version == "" || m.RolloutPercent < 0 || m.RolloutPercent > 100 {
		return nil, ErrBadFormat
	}
	return &m, nil
}

// Find returns the artifact for goos/goarch.
func (m *Manifest) Find(goos, goarch string) (*Artifact, bool) {
	for i := range m.Artifacts {
		if m.Artifacts[i].OS == goos && m.Artifacts[i].Arch == goarch {
			return &m.Artifacts[i], true
		}
	}
	return nil, false
}

// Bucket maps node+version to 0..99. Including the version reshuffles which
// nodes go first on each release (no permanent canaries).
func Bucket(nodeID, version string) int {
	h := sha256.Sum256([]byte(nodeID + "\x00" + version))
	return int(binary.BigEndian.Uint64(h[:8]) % 100)
}

// InRollout reports whether nodeID takes this release.
func InRollout(nodeID string, m *Manifest) bool {
	return Bucket(nodeID, m.Version) < m.RolloutPercent
}

// CompareVersions compares dotted numeric versions ("1.2.10" > "1.2.9"),
// ignoring a leading "v". Non-numeric parts compare as 0.
func CompareVersions(a, b string) int {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
