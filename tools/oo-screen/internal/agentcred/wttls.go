// SEC #32: TLS для легасі WebTransport — нормальна перевірка сертифіката за
// замовчуванням, пінінг SHA-256 сертифіката (-wt-cert-sha256) для
// самопідписаного hub-wt, і лише явний -wt-insecure вимикає перевірку.

package agentcred

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ParsePin розбирає SHA-256 сертифіката: 64 hex (двокрапки/пробіли дозволені)
// або base64 — саме так hub-wt друкує CERT_HASH=.
func ParsePin(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(strings.NewReplacer(":", "", " ", "").Replace(s)); err == nil && len(b) == sha256.Size {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == sha256.Size {
		return b, nil
	}
	return nil, errors.New("wt-cert-sha256: очікую SHA-256 сертифіката (64 hex або base64 з CERT_HASH= хаба)")
}

// WTTLSConfig — tls.Config для дзвінка hub-wt за адресою hubAddr (host:port).
//   - pin != nil: перевіряється ЛИШЕ відбиток leaf-сертифіката (самопідписаний
//     hub-wt без CA); ланцюг CA не потрібен.
//   - insecure: без перевірки взагалі (лише стенд).
//   - інакше: звичайна перевірка ланцюга й імені хоста.
func WTTLSConfig(hubAddr string, alpn []string, pin []byte, insecure bool) *tls.Config {
	host := hubAddr
	if h, _, err := net.SplitHostPort(hubAddr); err == nil {
		host = h
	}
	c := &tls.Config{ServerName: host, NextProtos: alpn, MinVersion: tls.VersionTLS13}
	switch {
	case pin != nil:
		want := append([]byte(nil), pin...)
		// Стандартну перевірку вимикаємо, бо сертифікат самопідписаний; замість
		// неї — точний збіг SHA-256 сертифіката сервера (certhash-пінінг).
		c.InsecureSkipVerify = true //nolint:gosec // замінено VerifyPeerCertificate нижче
		c.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("wt: сервер не надав сертифіката")
			}
			got := sha256.Sum256(raw[0])
			if !bytes.Equal(got[:], want) {
				return fmt.Errorf("wt: відбиток сертифіката %x не збігається з -wt-cert-sha256", got)
			}
			return nil
		}
	case insecure:
		c.InsecureSkipVerify = true //nolint:gosec // явний -wt-insecure (стенд)
	}
	return c
}
