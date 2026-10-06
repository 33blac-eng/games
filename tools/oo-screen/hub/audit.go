// audit.go — S4: журнал аудиту сесій (хто, коли, до якого ПК, скільки тривало,
// що робив). Append-only JSONL із хеш-ланцюгом: кожен рядок несе prev (хеш
// попереднього) і hash = sha256(prev || JSON запису з порожнім hash).
// Переписати/видалити/вставити рядок посередині файлу можна, але Verify це
// побачить на першому ж зламаному рядку. Обрізання ХВОСТА ланцюг сам по собі
// не ловить — для цього ЯКІР (хвиля 8, OO_SCREEN_AUDIT_ANCHOR=<файл>, дефолт
// вимкнено): після кожного запису хаб атомарно (tmp+rename, fsync) пише в
// окремий файл {"seq","hash"} останнього запису і той самий рядок — у журнал
// процесу (journald — друге, незалежне від файлу аудиту сховище). На старті
// ланцюг мусить ДОСЯГАТИ seq якоря і мати на ньому той самий hash, інакше
// хаб не стартує: обрізаний хвіст (або переписаний з того місця журнал)
// виявляється. Якір варто класти на інший диск/монтування, ніж журнал; той,
// хто може переписати обидва файли, якір обходить — тоді лишається журнал
// процесу (порівняти вручну: `journalctl | grep "audit anchor"`).
//
// Увімкнення: OO_SCREEN_AUDIT_LOG=<шлях> (дефолт — вимкнено, як і решта нових
// функцій). Читання: GET /admin/audit з Authorization: Bearer
// $OO_SCREEN_AUDIT_TOKEN; без токена ендпоінт не існує (404).
package hub

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AuditGenesis — prev першого запису.
const AuditGenesis = "0000000000000000000000000000000000000000000000000000000000000000"

// AuditRecord — один рядок журналу. Поля, яких подія не має, опускаються.
type AuditRecord struct {
	Seq        uint64           `json:"seq"`
	TS         string           `json:"ts"` // RFC3339Nano UTC
	Event      string           `json:"event"`
	Node       string           `json:"node,omitempty"`
	User       string           `json:"user,omitempty"`
	Grant      string           `json:"grant,omitempty"`
	Session    string           `json:"session,omitempty"`
	Remote     string           `json:"remote,omitempty"`
	DurationMS int64            `json:"duration_ms,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	Counts     map[string]int64 `json:"counts,omitempty"`
	Detail     string           `json:"detail,omitempty"`
	Prev       string           `json:"prev"`
	Hash       string           `json:"hash,omitempty"`
}

func auditHash(r AuditRecord) (string, error) {
	r.Hash = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(r.Prev))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// AuditLog — потокобезпечний дописувач.
type AuditLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	seq  uint64
	last string
	now  func() time.Time
	// anchor — файл якоря ("" — вимкнено); onAnchor — куди ще віддати якір
	// (журнал процесу), nil — нікуди.
	anchor   string
	onAnchor func(seq uint64, hash string)
}

// AuditAnchor — якір хвоста ланцюга.
type AuditAnchor struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}

// ErrAuditTruncated — ланцюг коротший за якір або розходиться з ним.
var ErrAuditTruncated = errors.New("audit: ланцюг не досягає якоря (обрізано хвіст або переписано)")

// ReadAuditAnchor читає якір; файлу нема — нульовий якір без помилки.
func ReadAuditAnchor(path string) (AuditAnchor, error) {
	var a AuditAnchor
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return a, fmt.Errorf("audit anchor %s: %w", path, err)
	}
	if a.Seq == 0 || len(a.Hash) != len(AuditGenesis) {
		return a, fmt.Errorf("audit anchor %s: порожній або битий", path)
	}
	return a, nil
}

// writeAuditAnchor — атомарно (tmp у тому ж каталозі + fsync + rename).
func writeAuditAnchor(path string, a AuditAnchor) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// OpenAuditLogAnchored — OpenAuditLog плюс якір хвоста anchorPath ("" —
// як OpenAuditLog). Ланцюг, що не досягає якоря або має на його seq інший
// hash, — ErrAuditTruncated. Немає файлу якоря — перший запуск з якорем:
// якір ставиться на поточний хвіст (і віддається onAnchor).
func OpenAuditLogAnchored(path, anchorPath string, onAnchor func(seq uint64, hash string)) (*AuditLog, error) {
	if anchorPath == "" {
		return OpenAuditLog(path)
	}
	anc, err := ReadAuditAnchor(anchorPath)
	if err != nil {
		return nil, err
	}
	if anc.Seq > 0 {
		var at string
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("%w: якір seq %d, журналу нема: %v", ErrAuditTruncated, anc.Seq, err)
		}
		seq, _, verr := verifyAudit(f, func(s uint64, h string) {
			if s == anc.Seq {
				at = h
			}
		})
		f.Close()
		if verr != nil {
			return nil, verr
		}
		if seq < anc.Seq || subtle.ConstantTimeCompare([]byte(at), []byte(anc.Hash)) != 1 {
			return nil, fmt.Errorf("%w: якір seq %d, у журналі %d записів", ErrAuditTruncated, anc.Seq, seq)
		}
	}
	a, err := OpenAuditLog(path)
	if err != nil {
		return nil, err
	}
	a.anchor, a.onAnchor = anchorPath, onAnchor
	if a.seq > 0 {
		if err := writeAuditAnchor(anchorPath, AuditAnchor{Seq: a.seq, Hash: a.last}); err != nil {
			a.Close()
			return nil, err
		}
		if onAnchor != nil {
			onAnchor(a.seq, a.last)
		}
	}
	return a, nil
}

// OpenAuditLog відкриває (або створює) журнал і перевіряє весь наявний
// ланцюг. Зламаний ланцюг = помилка: дописувати до підробленого журналу —
// значить легітимізувати підробку.
func OpenAuditLog(path string) (*AuditLog, error) {
	seq, last, err := VerifyAuditFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if last == "" {
		last = AuditGenesis
	}
	return &AuditLog{f: f, path: path, seq: seq, last: last, now: time.Now}, nil
}

// Append дописує запис (Seq/TS/Prev/Hash заповнює сам) і fsync-ить.
// nil-журнал — нічого не робить (аудит вимкнено).
func (a *AuditLog) Append(r AuditRecord) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r.Seq = a.seq + 1
	r.TS = a.now().UTC().Format(time.RFC3339Nano)
	r.Prev = a.last
	h, err := auditHash(r)
	if err != nil {
		return err
	}
	r.Hash = h
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := a.f.Write(b); err != nil {
		return err
	}
	if err := a.f.Sync(); err != nil {
		return err
	}
	a.seq, a.last = r.Seq, h
	if a.anchor != "" {
		// Запис уже на диску; якір відстає щонайбільше на цей рядок, і
		// старт це переживе (ланцюг довший за якір — норма).
		if err := writeAuditAnchor(a.anchor, AuditAnchor{Seq: r.Seq, Hash: h}); err != nil {
			return err
		}
		if a.onAnchor != nil {
			a.onAnchor(r.Seq, h)
		}
	}
	return nil
}

// Close закриває файл.
func (a *AuditLog) Close() error {
	if a == nil {
		return nil
	}
	return a.f.Close()
}

// VerifyAuditFile перевіряє ланцюг; повертає seq і hash останнього запису.
func VerifyAuditFile(path string) (uint64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	return VerifyAudit(f)
}

// VerifyAudit — те саме над довільним потоком.
func VerifyAudit(rd io.Reader) (uint64, string, error) {
	return verifyAudit(rd, nil)
}

// verifyAudit — VerifyAudit з колбеком на кожен перевірений запис.
func verifyAudit(rd io.Reader, each func(seq uint64, hash string)) (uint64, string, error) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	prev, seq := AuditGenesis, uint64(0)
	line := 0
	for sc.Scan() {
		line++
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var r AuditRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return seq, prev, fmt.Errorf("audit line %d: bad json: %w", line, err)
		}
		if r.Seq != seq+1 {
			return seq, prev, fmt.Errorf("audit line %d: seq %d, want %d", line, r.Seq, seq+1)
		}
		if r.Prev != prev {
			return seq, prev, fmt.Errorf("audit line %d: prev mismatch (chain broken)", line)
		}
		h, err := auditHash(r)
		if err != nil {
			return seq, prev, err
		}
		if subtle.ConstantTimeCompare([]byte(h), []byte(r.Hash)) != 1 {
			return seq, prev, fmt.Errorf("audit line %d: hash mismatch (record altered)", line)
		}
		prev, seq = r.Hash, r.Seq
		if each != nil {
			each(seq, prev)
		}
	}
	if err := sc.Err(); err != nil {
		return seq, prev, err
	}
	if seq == 0 {
		return 0, "", nil
	}
	return seq, prev, nil
}

// Handler — GET /admin/audit?node=&user=&since=<RFC3339>&limit=N[&verify=1].
// Доступ лише з Bearer-токеном адміна (порівняння за сталий час). Порожній
// токен або вимкнений журнал = 404, щоб забута змінна не відчинила журнал.
func (a *AuditLog) Handler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a == nil || token == "" {
			http.NotFound(w, r)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		limit := 1000
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 100000 {
			limit = v
		}
		var since time.Time
		if s := q.Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				http.Error(w, "bad since", http.StatusBadRequest)
				return
			}
			since = t
		}
		node, user := q.Get("node"), q.Get("user")
		a.mu.Lock() // знімок під локом: не читаємо напівзаписаний рядок
		data, err := os.ReadFile(a.path)
		a.mu.Unlock()
		if err != nil {
			http.Error(w, "read failed", http.StatusInternalServerError)
			return
		}
		out := struct {
			Records  []AuditRecord `json:"records"`
			Verified *bool         `json:"verified,omitempty"`
			Error    string        `json:"verify_error,omitempty"`
		}{Records: []AuditRecord{}}
		if q.Get("verify") == "1" {
			_, _, verr := VerifyAudit(bytes.NewReader(data))
			ok := verr == nil
			out.Verified = &ok
			if verr != nil {
				out.Error = verr.Error()
			}
		}
		for _, ln := range bytes.Split(data, []byte{'\n'}) {
			if len(bytes.TrimSpace(ln)) == 0 {
				continue
			}
			var rec AuditRecord
			if json.Unmarshal(ln, &rec) != nil {
				continue
			}
			if (node != "" && rec.Node != node) || (user != "" && rec.User != user) {
				continue
			}
			if !since.IsZero() {
				if t, err := time.Parse(time.RFC3339Nano, rec.TS); err == nil && t.Before(since) {
					continue
				}
			}
			out.Records = append(out.Records, rec)
		}
		if len(out.Records) > limit { // найсвіжіші
			out.Records = out.Records[len(out.Records)-limit:]
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// AuditSession — лічильники однієї viewer-сесії; кінець пишеться рівно раз.
type AuditSession struct {
	log   *AuditLog
	base  AuditRecord
	start time.Time
	mu    sync.Mutex
	cnt   map[string]int64
	ended bool
}

// StartSession пише session_start і повертає трекер. nil-журнал → nil-трекер,
// усі методи якого — no-op.
func (a *AuditLog) StartSession(base AuditRecord) *AuditSession {
	if a == nil {
		return nil
	}
	s := &AuditSession{log: a, base: base, start: time.Now(), cnt: map[string]int64{}}
	rec := base
	rec.Event = "session_start"
	_ = a.Append(rec)
	return s
}

// Count додає n до лічильника name (input_accepted, input_dropped, …).
func (s *AuditSession) Count(name string, n int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.ended {
		s.cnt[name] += n
	}
	s.mu.Unlock()
}

// End пише session_end із тривалістю й підсумком подій. Повторні — no-op.
func (s *AuditSession) End(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	cnt := make(map[string]int64, len(s.cnt))
	for k, v := range s.cnt {
		cnt[k] = v
	}
	s.mu.Unlock()
	rec := s.base
	rec.Event = "session_end"
	rec.Reason = reason
	rec.DurationMS = time.Since(s.start).Milliseconds()
	if len(cnt) > 0 {
		rec.Counts = cnt
	}
	_ = s.log.Append(rec)
}
