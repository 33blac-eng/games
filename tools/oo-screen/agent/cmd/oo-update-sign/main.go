// oo-update-sign: S6 release tooling.
//
//	oo-update-sign -genkey                      # prints PRIVATE (base64 seed) and PUBLIC (hex)
//	OO_UPDATE_SIGNING_KEY=<seed b64> oo-update-sign -exe dist/oo-agent-windows-amd64.exe \
//	    -version 1.4.0 -url https://hub/updates/oo-agent-1.4.0.exe -rollout 10 -out manifest.json
//
// The private key is read ONLY from env OO_UPDATE_SIGNING_KEY (never a flag:
// it would be visible in the process list / CI logs).
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "oo-update-sign:", err)
		os.Exit(1)
	}
}

func run() error {
	genkey := flag.Bool("genkey", false, "generate a new key pair and exit")
	exe := flag.String("exe", "", "agent binary to publish")
	version := flag.String("version", "", "semver of the binary (must match its -X main.agentVersion)")
	url := flag.String("url", "", "download URL of the binary")
	rollout := flag.Int("rollout", 10, "rollout percent 0..100")
	channel := flag.String("channel", "stable", "channel label")
	goos := flag.String("os", "windows", "artifact GOOS")
	goarch := flag.String("arch", "amd64", "artifact GOARCH")
	out := flag.String("out", "manifest.json", "output file")
	flag.Parse()

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		fmt.Println("PRIVATE (secret OO_UPDATE_SIGNING_KEY):", base64.StdEncoding.EncodeToString(priv.Seed()))
		fmt.Println("PUBLIC  (build OO_UPDATE_PUBKEY):     ", hex.EncodeToString(pub))
		return nil
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("OO_UPDATE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("OO_UPDATE_SIGNING_KEY must be a base64 %d-byte seed", ed25519.SeedSize)
	}
	if *exe == "" || *version == "" || *url == "" {
		return fmt.Errorf("-exe, -version and -url are required")
	}
	if *rollout < 0 || *rollout > 100 {
		return fmt.Errorf("-rollout must be 0..100")
	}
	b, err := os.ReadFile(*exe)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	priv := ed25519.NewKeyFromSeed(seed)
	env, err := autoupdate.Sign(priv, autoupdate.Manifest{
		Product: autoupdate.Product, Version: *version, Channel: *channel,
		RolloutPercent: *rollout, IssuedAt: time.Now().UTC().Truncate(time.Second),
		Artifacts: []autoupdate.Artifact{{OS: *goos, Arch: *goarch, URL: *url,
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}},
	})
	if err != nil {
		return err
	}
	// Self-check with the derived public key before writing.
	if _, err := autoupdate.Verify(priv.Public().(ed25519.PublicKey), env); err != nil {
		return err
	}
	if err := os.WriteFile(*out, env, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s %s (%d bytes, rollout %d%%) public key %s\n", *out, *version, len(b), *rollout,
		hex.EncodeToString(priv.Public().(ed25519.PublicKey)))
	return nil
}
