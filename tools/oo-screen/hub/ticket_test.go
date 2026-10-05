package hub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestConsumeTicket_OK(t *testing.T) {
	var gotPath, gotKey, gotTicket string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-OO-Hub-Key")
		body, _ := io.ReadAll(r.Body)
		var req consumeTicketRequest
		_ = json.Unmarshal(body, &req)
		gotTicket = req.Ticket
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(TicketClaims{
			UserID: "42",
			OrgID:  "1",
			NodeID: "node-A",
			Grant:  "view",
		})
	}))
	defer srv.Close()

	claims, err := ConsumeTicket(srv.URL, "secret-key", "jti-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/remote-access/screen/internal/consume" {
		t.Errorf("path = %q, want /remote-access/screen/internal/consume", gotPath)
	}
	if gotKey != "secret-key" {
		t.Errorf("X-OO-Hub-Key = %q, want secret-key", gotKey)
	}
	if gotTicket != "jti-123" {
		t.Errorf("ticket in body = %q, want jti-123", gotTicket)
	}
	if claims.UserID != "42" || claims.OrgID != "1" || claims.NodeID != "node-A" || claims.Grant != "view" {
		t.Errorf("claims = %+v, unexpected", claims)
	}
}

func TestConsumeTicket_TrailingSlashInBase(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TicketClaims{UserID: "1", OrgID: "1"})
	}))
	defer srv.Close()

	if _, err := ConsumeTicket(srv.URL+"/", "k", "j"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/remote-access/screen/internal/consume" {
		t.Errorf("path = %q, want /remote-access/screen/internal/consume (no double slash)", gotPath)
	}
}

func TestConsumeTicket_ErpRejects403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"ticket already consumed"}`))
	}))
	defer srv.Close()

	_, err := ConsumeTicket(srv.URL, "k", "replayed-jti")
	if err == nil {
		t.Fatal("expected error on 403, got nil")
	}
	te, ok := err.(*TicketError)
	if !ok {
		t.Fatalf("expected *TicketError, got %T: %v", err, err)
	}
	if te.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", te.StatusCode)
	}
	if !strings.Contains(te.Error(), "403") {
		t.Errorf("Error() = %q, want mention of 403", te.Error())
	}
}

func TestConsumeTicket_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	if _, err := ConsumeTicket(srv.URL, "k", "j"); err == nil {
		t.Fatal("expected error on malformed JSON body, got nil")
	}
}

func TestConsumeTicket_EmptyClaims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	if _, err := ConsumeTicket(srv.URL, "k", "j"); err == nil {
		t.Fatal("expected error on empty claims body, got nil (fail-open bug)")
	}
}

func TestConsumeTicket_EmptyArgs(t *testing.T) {
	if _, err := ConsumeTicket("", "k", "j"); err == nil {
		t.Error("expected error on empty erpBase")
	}
	if _, err := ConsumeTicket("http://example.invalid", "k", ""); err == nil {
		t.Error("expected error on empty ticket")
	}
}

func TestConsumeTicket_Unreachable(t *testing.T) {
	// Порт, на якому нічого не слухає — маємо отримати мережеву помилку,
	// не паніку і не false-positive claims.
	_, err := ConsumeTicket("http://127.0.0.1:1", "k", "j")
	if err == nil {
		t.Fatal("expected network error, got nil")
	}
}

// truncBody ріже тіло до 256 байтів, але не посеред символу: 255 байтів ASCII
// + кирилиця — межа 256 припадає на середину «ї».
func TestTruncBodyKeepsUTF8(t *testing.T) {
	body := []byte(strings.Repeat("a", 255) + strings.Repeat("ї", 10))
	got := truncBody(body)
	if !utf8.ValidString(got) {
		t.Fatalf("обрізане тіло — невалідний UTF-8: %q", got[len(got)-8:])
	}
	if want := strings.Repeat("a", 255) + "…"; got != want {
		t.Errorf("got %q…, want 255×a + …", got[250:])
	}
	if got := truncBody([]byte("коротко")); got != "коротко" {
		t.Errorf("коротке тіло змінене: %q", got)
	}
}

// TestConsumeTicket_BadBodyTruncatedInError — R6-G6: 2xx з кривим тілом (ERP
// віддав сторінку помилки PHP зі статусом 200). Текст помилки йде в журнал
// (authorizeViewer), тож тіло в ньому мусить бути обрізане truncBody, а не
// до 1 МБ на кожен квиток. Постав string(respBody) назад — довжина вилізе.
func TestConsumeTicket_BadBodyTruncatedInError(t *testing.T) {
	page := strings.Repeat("<div>Whoops</div>", 4096) // ~68 КБ
	for _, body := range []string{page, `{"x":1,"pad":"` + page + `"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		_, err := ConsumeTicket(srv.URL, "k", "j")
		srv.Close()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if n := len(err.Error()); n > 1024 {
			t.Fatalf("помилка несе %d байт тіла ERP — у журнал поїде вся сторінка", n)
		}
	}
}
