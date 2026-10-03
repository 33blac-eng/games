// ticket.go — консюмер ERP single-use screen-ticket. Hub НЕ валідує JWT сам:
// довіряє ERP-ендпоінту (ScreenEngineController hub-consume), автентифікованому
// спільним секретом X-OO-Hub-Key. Consume — одноразовий: другий виклик з тим
// самим jti має повернути помилку на боці ERP (replay-захист там, не тут).
package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// TicketClaims — те, що повертає ERP при успішному consume single-use тікета.
// Grant — довільний рядок-опис дозволу (наприклад "view"/"control"), hub його
// не інтерпретує зараз, лише передає для майбутнього логування.
type TicketClaims struct {
	UserID string `json:"user_id"`
	OrgID  string `json:"org_id"`
	NodeID string `json:"node_id"`
	Grant  string `json:"grant"`
}

// TicketError — помилка consume з кодом HTTP-статусу ERP-відповіді (0, якщо
// запит взагалі не дійшов).
type TicketError struct {
	StatusCode int
	Body       string
}

// maxLoggedBody — стеля тексту ERP-відповіді, що потрапляє в помилку (а далі
// в journald). SEC #28: тіло могло б віддзеркалити квиток чи нутрощі ERP.
const maxLoggedBody = 200

// redactBody готує тіло ERP-відповіді для помилки/журналу: вирізає квиток
// (jti) і все, що схоже на bearer/JWT, і обрізає до maxLoggedBody символів.
func redactBody(body, jti string) string {
	if jti != "" {
		body = strings.ReplaceAll(body, jti, "[redacted]")
	}
	body = jwtLike.ReplaceAllString(body, "[redacted]")
	body = strings.ToValidUTF8(body, "?")
	if r := []rune(body); len(r) > maxLoggedBody {
		body = string(r[:maxLoggedBody]) + "…(truncated)"
	}
	return body
}

// jwtLike — три base64url-сегменти через крапку (JWT) або довгі hex/base64-рядки
// (≥ 32 символи) — типовий вигляд токенів і секретів.
var jwtLike = regexp.MustCompile(`[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}|[A-Za-z0-9+/_=-]{32,}`)

func (e *TicketError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("consume ticket: request failed: %s", e.Body)
	}
	return fmt.Sprintf("consume ticket: erp status %d: %s", e.StatusCode, e.Body)
}

// consumeTicketRequest — тіло POST на ERP hub-consume ендпоінт.
type consumeTicketRequest struct {
	Ticket string `json:"ticket"`
}

// ConsumeTicket викликає ERP {erpBase}/remote-access/screen/internal/consume із jti квитка
// та шляховим заголовком X-OO-Hub-Key. Повертає claims при 2xx-відповіді з
// валідним JSON-тілом; в усіх інших випадках — помилку (fail-closed).
func ConsumeTicket(erpBase, hubKey, jti string) (*TicketClaims, error) {
	if strings.TrimSpace(erpBase) == "" {
		return nil, fmt.Errorf("consume ticket: erpBase порожній")
	}
	if strings.TrimSpace(jti) == "" {
		return nil, fmt.Errorf("consume ticket: ticket порожній")
	}

	body, err := json.Marshal(consumeTicketRequest{Ticket: jti})
	if err != nil {
		return nil, fmt.Errorf("consume ticket: marshal: %w", err)
	}

	url := strings.TrimRight(erpBase, "/") + "/remote-access/screen/internal/consume"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("consume ticket: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OO-Hub-Key", hubKey)

	// H-32: той самий спільний транспорт, що й у поллінгу відкликань — квиток
	// споживається на кожному підключенні глядача, і піднімати заради цього
	// окреме TLS-зʼєднання марно.
	client := &http.Client{Timeout: 8 * time.Second, Transport: sharedERPTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &TicketError{StatusCode: 0, Body: redactBody(err.Error(), jti)}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &TicketError{StatusCode: resp.StatusCode, Body: redactBody(string(respBody), jti)}
	}

	var claims TicketClaims
	if err := json.Unmarshal(respBody, &claims); err != nil {
		// SEC #28: тіло 2xx не логуємо взагалі — лише довжину.
		return nil, fmt.Errorf("consume ticket: bad json from erp: %w (%d bytes)", err, len(respBody))
	}
	if strings.TrimSpace(claims.UserID) == "" && strings.TrimSpace(claims.OrgID) == "" {
		// ERP відповів 2xx, але без розпізнаваних claims — трактуємо як
		// невалідну відповідь, а не як «порожній, але легітимний» тікет.
		return nil, fmt.Errorf("consume ticket: erp response has no claims (%d bytes)", len(respBody))
	}
	return &claims, nil
}
