package autoupdate

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Updater holds everything one agent needs to check/stage/swap/verify.
type Updater struct {
	ExePath        string            // running binary (os.Executable())
	PubKey         ed25519.PublicKey // pinned at build time
	NodeID         string            // rollout bucket key
	CurrentVersion string
	ManifestURL    string
	GOOS, GOARCH   string
	Client         *http.Client // nil -> 60s timeout client
	MaxBytes       int64        // artifact cap; 0 -> 256 MiB
	// MaxStarts: starts of the new binary without reaching healthy before an
	// automatic rollback (crash-loop guard). 0 -> 3.
	MaxStarts int
}

// Pending is the on-disk marker written by Swap and consumed by Startup.
type Pending struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	Starts    int       `json:"starts"`
	SwappedAt time.Time `json:"swapped_at"`
}

// Errors from Check.
var (
	ErrNotNewer      = errors.New("autoupdate: manifest version not newer")
	ErrNotInRollout  = errors.New("autoupdate: node not in rollout bucket")
	ErrNoArtifact    = errors.New("autoupdate: no artifact for this os/arch")
	ErrDigest        = errors.New("autoupdate: artifact sha256/size mismatch")
	ErrUpdatePending = errors.New("autoupdate: previous update still pending")
)

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (u *Updater) maxBytes() int64 {
	if u.MaxBytes > 0 {
		return u.MaxBytes
	}
	return 256 << 20
}

func (u *Updater) markerPath() string { return u.ExePath + ".update.json" }
func (u *Updater) oldPath() string    { return u.ExePath + ".old" }
func (u *Updater) newPath() string    { return u.ExePath + ".new" }

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("autoupdate: GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("autoupdate: %s larger than %d bytes", url, limit)
	}
	return b, nil
}

// Check fetches and verifies the manifest and decides whether this node
// should update now.
func (u *Updater) Check(ctx context.Context) (*Manifest, *Artifact, error) {
	if _, err := os.Stat(u.markerPath()); err == nil {
		return nil, nil, ErrUpdatePending
	}
	env, err := u.get(ctx, u.ManifestURL, 1<<20)
	if err != nil {
		return nil, nil, err
	}
	m, err := Verify(u.PubKey, env)
	if err != nil {
		return nil, nil, err
	}
	if CompareVersions(m.Version, u.CurrentVersion) <= 0 {
		return m, nil, ErrNotNewer
	}
	if !InRollout(u.NodeID, m) {
		return m, nil, ErrNotInRollout
	}
	a, ok := m.Find(u.GOOS, u.GOARCH)
	if !ok || a.Size <= 0 || len(a.SHA256) != 64 {
		return m, nil, ErrNoArtifact
	}
	return m, a, nil
}

// Stage downloads the artifact and writes <exe>.new after checking size and
// sha256 against the signed manifest.
func (u *Updater) Stage(ctx context.Context, a *Artifact) error {
	if a.Size > u.maxBytes() {
		return fmt.Errorf("autoupdate: artifact %d bytes exceeds cap", a.Size)
	}
	b, err := u.get(ctx, a.URL, a.Size)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) != a.Size || !strings.EqualFold(hex.EncodeToString(sum[:]), a.SHA256) {
		return ErrDigest
	}
	tmp := u.newPath() + ".part"
	if err := os.WriteFile(tmp, b, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, u.newPath())
}

// Swap installs <exe>.new as <exe>, keeps the old one as <exe>.old and
// writes the pending marker. Running binaries can be renamed on Windows, so
// this works while the agent runs; the caller then restarts the process.
func (u *Updater) Swap(to string) error {
	if _, err := os.Stat(u.newPath()); err != nil {
		return err
	}
	_ = os.Remove(u.oldPath())
	if err := os.Rename(u.ExePath, u.oldPath()); err != nil {
		return err
	}
	if err := os.Rename(u.newPath(), u.ExePath); err != nil {
		_ = os.Rename(u.oldPath(), u.ExePath)
		return err
	}
	return u.writeMarker(Pending{From: u.CurrentVersion, To: to, SwappedAt: time.Now().UTC()})
}

func (u *Updater) writeMarker(p Pending) error {
	b, _ := json.Marshal(p)
	tmp := u.markerPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, u.markerPath())
}

func (u *Updater) readMarker() (*Pending, error) {
	b, err := os.ReadFile(u.markerPath())
	if err != nil {
		return nil, err
	}
	var p Pending
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Rollback restores <exe>.old over <exe> and removes the marker. The caller
// must exit so the supervisor (scheduled task) starts the old binary.
func (u *Updater) Rollback() error {
	if _, err := os.Stat(u.oldPath()); err != nil {
		_ = os.Remove(u.markerPath())
		return fmt.Errorf("autoupdate: rollback impossible, %s missing: %w", u.oldPath(), err)
	}
	bad := u.ExePath + ".bad"
	_ = os.Remove(bad)
	if err := os.Rename(u.ExePath, bad); err != nil {
		return err
	}
	if err := os.Rename(u.oldPath(), u.ExePath); err != nil {
		_ = os.Rename(bad, u.ExePath)
		return err
	}
	return os.Remove(u.markerPath())
}

// Commit accepts the new binary: drops <exe>.old and the marker.
func (u *Updater) Commit() error {
	_ = os.Remove(u.oldPath())
	_ = os.Remove(u.ExePath + ".bad")
	return os.Remove(u.markerPath())
}

// StartupResult tells the caller what Startup did.
type StartupResult int

const (
	NoPending  StartupResult = iota // nothing to verify
	Committed                       // new binary healthy, kept
	RolledBack                      // old binary restored; caller must exit
)

// Startup runs at every agent start. If a swap is pending it counts the
// start, runs health (bounded by ctx) and commits or rolls back.
func (u *Updater) Startup(ctx context.Context, health func(context.Context) error) (StartupResult, error) {
	p, err := u.readMarker()
	if errors.Is(err, os.ErrNotExist) {
		return NoPending, nil
	}
	if err != nil { // unreadable marker: be conservative
		return RolledBack, u.Rollback()
	}
	maxStarts := u.MaxStarts
	if maxStarts <= 0 {
		maxStarts = 3
	}
	p.Starts++
	if p.Starts > maxStarts {
		return RolledBack, u.Rollback()
	}
	if err := u.writeMarker(*p); err != nil {
		return NoPending, err
	}
	if herr := health(ctx); herr != nil {
		if rerr := u.Rollback(); rerr != nil {
			return RolledBack, errors.Join(herr, rerr)
		}
		return RolledBack, herr
	}
	return Committed, u.Commit()
}

// CleanupStale removes leftovers of an interrupted Stage.
func (u *Updater) CleanupStale() {
	_ = os.Remove(u.newPath() + ".part")
	if _, err := os.Stat(u.markerPath()); errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(u.newPath())
	}
}
