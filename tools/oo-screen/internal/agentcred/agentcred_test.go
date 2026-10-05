package agentcred

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// SEC #33: токен із файлу / env важливіший за -token; битий файл — помилка.
func TestResolveTokenOrder(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if tok, src, _ := ResolveToken("", "", get); tok != DevToken || src != SourceDev {
		t.Fatalf("дефолт: %q %s", tok, src)
	}
	env["OO_SCREEN_T1_TOKEN"] = "legacy"
	if tok, src, _ := ResolveToken("", "", get); tok != "legacy" || src != SourceLegacyE {
		t.Fatalf("легасі env: %q %s", tok, src)
	}
	if tok, src, _ := ResolveToken("cli", "", get); tok != "cli" || src != SourceFlag {
		t.Fatalf("-token: %q %s", tok, src)
	}
	env["OO_AGENT_TOKEN"] = " envtok\n"
	if tok, src, _ := ResolveToken("cli", "", get); tok != "envtok" || src != SourceEnv {
		t.Fatalf("OO_AGENT_TOKEN: %q %s", tok, src)
	}
	p := filepath.Join(t.TempDir(), "agent.token")
	if err := os.WriteFile(p, []byte("filetok\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, src, err := ResolveToken("cli", p, get); err != nil || tok != "filetok" || src != SourceFile {
		t.Fatalf("файл: %q %s %v", tok, src, err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	_ = os.WriteFile(empty, []byte(" \n"), 0o600)
	if _, _, err := ResolveToken("cli", empty, get); err == nil {
		t.Fatal("порожній файл прийнято")
	}
	if _, _, err := ResolveToken("cli", empty+".missing", get); err == nil {
		t.Fatal("відсутній файл прийнято — тихий фолбек на слабше джерело")
	}
}

// SEC #32: дефолт — повна перевірка; пін — точний збіг відбитка; insecure — лише явно.
func TestWTTLSConfig(t *testing.T) {
	c := WTTLSConfig("hub.example:4460", []string{"a"}, nil, false)
	if c.InsecureSkipVerify || c.ServerName != "hub.example" || c.VerifyPeerCertificate != nil {
		t.Fatalf("дефолт без перевірки: %+v", c)
	}
	if !WTTLSConfig("h:1", nil, nil, true).InsecureSkipVerify {
		t.Fatal("-wt-insecure не вимкнув перевірку")
	}
	der := []byte("fake-cert-der")
	sum := sha256.Sum256(der)
	pin, err := ParsePin(base64.StdEncoding.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if p2, err := ParsePin(hex.EncodeToString(sum[:])); err != nil || !bytes.Equal(p2, pin) {
		t.Fatalf("hex-пін: %v", err)
	}
	if _, err := ParsePin("abcd"); err == nil {
		t.Fatal("короткий пін прийнято")
	}
	c = WTTLSConfig("h:1", nil, pin, false)
	if err := c.VerifyPeerCertificate([][]byte{der}, nil); err != nil {
		t.Fatalf("свій сертифікат відхилено: %v", err)
	}
	if err := c.VerifyPeerCertificate([][]byte{[]byte("evil")}, nil); err == nil {
		t.Fatal("чужий сертифікат прийнято")
	}
	if err := c.VerifyPeerCertificate(nil, nil); err == nil {
		t.Fatal("порожній ланцюг прийнято")
	}
}
