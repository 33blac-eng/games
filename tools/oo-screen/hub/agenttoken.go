// agenttoken.go — SEC #17: агентські облікові дані на ноду. Токен ноди =
// hex(HMAC-SHA256(master, nodeID)). Хаб звіряє його сталим часом; витік токена
// з одного ПК більше не дає права зареєструватись під ЧУЖОЮ нодою.
package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
)

// NodeToken — агентський токен ноди nodeID, виведений із master-секрету.
func NodeToken(master, nodeID string) string {
	m := hmac.New(sha256.New, []byte(master))
	m.Write([]byte("oo-screen-agent/v1\x00"))
	m.Write([]byte(nodeID))
	return hex.EncodeToString(m.Sum(nil))
}

// NodeTokenValid — чи got є токеном саме ноди nodeID (constant-time).
// Порожній master чи nodeID не валідний ніколи.
func NodeTokenValid(master, nodeID, got string) bool {
	if master == "" || nodeID == "" || got == "" {
		return false
	}
	want := NodeToken(master, nodeID)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
