// hub-wt — hub процес кандидата B (WebTransport), T1-стенд.
//
// Дві ноги:
//   - QUIC ingest від агента на :4460/udp, ALPN "oo-screen-agent". Перший
//     стрім — control (JSON {"token":"..."}), другий стрім (за epoch) —
//     потік envelope-кадрів (internal/envelope.ReadFrame).
//   - WebTransport на :4461/udp, шлях /wt, ?token=... . Глядач отримує
//     один uni-stream, на який hub ретранслює ті самі байти конверта.
//
// T1 scope: рівно 1 агент + 1 глядач. Агент-реєстр — один слот з
// generation-лічильником; compare-and-delete при переприєднанні не дає
// застарілому агенту затерти нового.
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/organicoils/oo-screen/hub"
	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/envelope"
)

const (
	agentListenAddr     = ":4460"
	wtListenAddr        = ":4461"
	agentALPN           = "oo-screen-agent"
	relayBufLen         = 120 // ~2s @60fps — з запасом на джиттер до наступного IDR
	heartbeatInterval   = 5 * time.Second
	keyframeReqDebounce = 500 * time.Millisecond
)

var startTime = time.Now()

func monoMs() int64 { return time.Since(startTime).Milliseconds() }

func authToken() string {
	if v := os.Getenv("OO_SCREEN_T1_TOKEN"); v != "" {
		return v
	}
	return "t1-dev-token"
}

// agentRegistry — єдиний слот агента з generation-лічильником для T1.
type agentRegistry struct {
	mu   sync.Mutex
	gen  uint64
	conn *quic.Conn // поточне з'єднання агента (nil, якщо немає)
}

// Register реєструє нове з'єднання агента, закриваючи попереднє (якщо є),
// і повертає generation цього підключення для compare-and-delete при виході.
func (a *agentRegistry) Register(conn *quic.Conn) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn != nil {
		log.Printf("hub-wt: new agent connection, closing stale one (gen=%d)", a.gen)
		_ = a.conn.CloseWithError(0, "superseded")
	}
	a.gen++
	a.conn = conn
	return a.gen
}

// Unregister прибирає з'єднання, лише якщо generation досі відповідає —
// стале з'єднання, що завершується пізніше, не зачепить нового агента.
func (a *agentRegistry) Unregister(gen uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gen == gen {
		a.conn = nil
	}
}

func main() {
	registry := &agentRegistry{}
	relay := hub.NewRelayBuf(relayBufLen)
	agentCtrl := &agentControl{}

	certDER, certKey, err := genCert()
	if err != nil {
		log.Fatalf("hub-wt: cert generation failed: %v", err)
	}
	hash := sha256.Sum256(certDER)
	// canonical: base64 — узгоджено з viewer-wt (RTCCertificate/serverCertificateHashes
	// приймає base64 у сторінці) і capture.py (парсить CERT_HASH= як base64);
	// НЕ переводити на hex тут без узгодженої зміни в обох.
	certHash := base64.StdEncoding.EncodeToString(hash[:])
	fmt.Printf("CERT_HASH=%s\n", certHash)

	tlsCert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: certKey}

	go func() {
		if err := runAgentIngest(tlsCert, registry, relay, agentCtrl); err != nil {
			log.Fatalf("hub-wt: agent ingest listener failed: %v", err)
		}
	}()

	if err := runWebTransport(tlsCert, relay, agentCtrl); err != nil {
		log.Fatalf("hub-wt: webtransport listener failed: %v", err)
	}
}

// agentControl тримає посилання на персистентний control-стрім поточного
// агента (перший стрім його QUIC-з'єднання, живий після hello) і дозволяє
// hub-у писати в нього keyframe_request — від viewer-а або з логіки
// приєднання глядача (TrimToLatestKeyframe без keyframe у буфері).
// Debounce 500мс: кілька запитів поспіль (від різних тригерів) не мають
// закидати агента дублікатами.
type agentControl struct {
	mu              sync.Mutex
	stream          *quic.Stream
	seq             atomic.Uint64
	lastKeyframeReq time.Time
}

func (a *agentControl) SetStream(s *quic.Stream) {
	a.mu.Lock()
	a.stream = s
	a.mu.Unlock()
}

// ClearStream прибирає стрім, лише якщо він досі саме той, що передавався —
// уникає ситуації, коли стара (вже замінена) горутина очищає щойно
// зареєстрований стрім нового агента.
func (a *agentControl) ClearStream(s *quic.Stream) {
	a.mu.Lock()
	if a.stream == s {
		a.stream = nil
	}
	a.mu.Unlock()
}

// RequestKeyframe шле keyframe_request агенту, якщо стрім живий і з
// попереднього запиту минуло не менше keyframeReqDebounce.
func (a *agentControl) RequestKeyframe() {
	// Знімок стріму й дебаунс-рішення під локом; сам запис — блокуючий I/O,
	// тому виконуємо його ПОЗА локом (інакше повільний/залиплий агент
	// тримає мʼютекс і блокує кожного viewer-а/тригера, що теж хоче
	// смикнути RequestKeyframe чи SetStream/ClearStream з іншої горутини).
	a.mu.Lock()
	s := a.stream
	if s == nil {
		a.mu.Unlock()
		log.Printf("hub-wt: keyframe_request suppressed: no live agent control stream")
		return
	}
	if since := time.Since(a.lastKeyframeReq); since < keyframeReqDebounce {
		a.mu.Unlock()
		log.Printf("hub-wt: keyframe_request debounced (last one %v ago)", since)
		return
	}
	a.lastKeyframeReq = time.Now()
	seq := a.seq.Add(1)
	a.mu.Unlock()

	_ = s.SetWriteDeadline(time.Now().Add(2 * time.Second))
	err := control.Write(s, control.KeyframeRequest(seq))
	_ = s.SetWriteDeadline(time.Time{})
	if err != nil {
		log.Printf("hub-wt: keyframe_request write to agent failed: %v", err)
		return
	}
	log.Printf("hub-wt: sent keyframe_request seq=%d to agent", seq)
}

func genCert() ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	notBefore := now.Add(-1 * time.Hour)
	notAfter := notBefore.Add(13 * 24 * time.Hour) // < 14 днів (браузерна вимога)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return der, key, nil
}

// --- Agent ingest (plain QUIC, ALPN oo-screen-agent) ---

func runAgentIngest(cert tls.Certificate, registry *agentRegistry, relay *hub.RelayBuf, agentCtrl *agentControl) error {
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{agentALPN},
	}
	ln, err := quic.ListenAddr(agentListenAddr, tlsConf, &quic.Config{})
	if err != nil {
		return err
	}
	log.Printf("hub-wt: agent ingest listening on %s (ALPN=%s)", agentListenAddr, agentALPN)
	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			return err
		}
		go handleAgentConn(conn, registry, relay, agentCtrl)
	}
}

const controlHelloTimeout = 5 * time.Second

func handleAgentConn(conn *quic.Conn, registry *agentRegistry, relay *hub.RelayBuf, agentCtrl *agentControl) {
	// Аутентифікація ДО реєстрації: неавтентифікований конект не повинен
	// мати змоги витіснити справжнього вже зареєстрованого агента.
	ctx := context.Background()

	// Перший стрім — control. Лишається відкритим на все з'єднання
	// (персистентний bidi): після hello ним іде heartbeat від агента і
	// keyframe_request від hub-а.
	ctrlStr, err := conn.AcceptStream(ctx)
	if err != nil {
		log.Printf("hub-wt: agent conn: control stream accept failed: %v", err)
		conn.CloseWithError(0, "")
		return
	}
	br := bufio.NewReader(ctrlStr)

	_ = ctrlStr.SetReadDeadline(time.Now().Add(controlHelloTimeout))
	hello, err := control.Read(br)
	if err != nil {
		log.Printf("hub-wt: agent conn: control read failed: %v", err)
		conn.CloseWithError(0, "")
		return
	}
	if hello.Type != control.TypeHello {
		log.Printf("hub-wt: agent conn: expected hello, got type=%q", hello.Type)
		conn.CloseWithError(1, "expected hello")
		return
	}
	if hello.Token != authToken() {
		log.Printf("hub-wt: agent conn: bad token")
		conn.CloseWithError(2, "unauthorized")
		return
	}
	_ = ctrlStr.SetReadDeadline(time.Time{}) // знімаємо дедлайн — далі heartbeat кожні 5с

	// Аутентифіковано — тепер можна реєструвати (і, якщо треба, витісняти
	// попереднього агента).
	gen := registry.Register(conn)
	defer registry.Unregister(gen)
	defer conn.CloseWithError(0, "")
	log.Printf("hub-wt: agent gen=%d authenticated", gen)

	agentCtrl.SetStream(ctrlStr)
	defer agentCtrl.ClearStream(ctrlStr)

	// Персистентне читання control-стріму агента: heartbeat раз/5с, інші
	// відомі типи — логуються, невідомі — ігноруються (control.ReadKnown).
	go func() {
		for {
			_ = ctrlStr.SetReadDeadline(time.Now().Add(3 * heartbeatInterval))
			m, err := control.ReadKnown(br, nil)
			if err != nil {
				log.Printf("hub-wt: agent gen=%d: control stream done: %v, closing conn", gen, err)
				// Control-стрім мертвий (heartbeat-таймаут або обрив) — сам
				// по собі AcceptStream на новий відео-стрім (main-горутина
				// handleAgentConn) цього не побачить і конект лишиться
				// зареєстрованим "живим" назавжди. Закриваємо весь QUIC
				// конект: AcceptStream там поверне помилку, функція вийде
				// і defer registry.Unregister(gen) спрацює.
				conn.CloseWithError(4, "control stream dead")
				return
			}
			switch m.Type {
			case control.TypeHeartbeat:
				log.Printf("hub-wt: agent gen=%d heartbeat seq=%d", gen, m.Seq)
			default:
				log.Printf("hub-wt: agent gen=%d control msg type=%s seq=%d", gen, m.Type, m.Seq)
			}
		}
	}()

	// Наступні стріми — по одному відео-стріму на epoch.
	var seq atomic.Uint64
	for {
		videoStr, err := conn.AcceptStream(ctx)
		if err != nil {
			log.Printf("hub-wt: agent gen=%d: connection done: %v", gen, err)
			return
		}
		go func() {
			for {
				f, err := envelope.ReadFrame(videoStr)
				if err != nil {
					if !errors.Is(err, io.EOF) {
						// Додаток D: framing-помилка envelope — session-fatal.
						// Не лишаємо конект зареєстрованим з мертвим стрімом —
						// закриваємо весь QUIC-конект агента.
						log.Printf("hub-wt: agent gen=%d: video stream framing error, closing conn: %v", gen, err)
						conn.CloseWithError(3, "framing error")
					}
					return
				}
				n := seq.Add(1)
				logNDJSON("agent", int64(f.FrameSeq), monoMs())
				_ = n
				relay.Push(f)
			}
		}()
	}
}

var ndjsonMu sync.Mutex

func logNDJSON(leg string, seq int64, tMs int64) {
	b, _ := json.Marshal(struct {
		Leg string `json:"leg"`
		Seq int64  `json:"seq"`
		TMs int64  `json:"t_ms"`
	}{leg, seq, tMs})
	// Один синхронізований запис рядка з \n — інакше конкурентні виклики
	// з різних горутин (agent-нога і viewer-нога) можуть інтерлівитись і
	// зіпсувати NDJSON.
	b = append(b, '\n')
	ndjsonMu.Lock()
	os.Stdout.Write(b)
	ndjsonMu.Unlock()
}

// --- WebTransport (viewer, :4461/wt) ---

func runWebTransport(cert tls.Certificate, relay *hub.RelayBuf, agentCtrl *agentControl) error {
	tlsConf := &tls.Config{Certificates: []tls.Certificate{cert}}

	h3Server := &http3.Server{
		Addr:      wtListenAddr,
		TLSConfig: http3.ConfigureTLSConfig(tlsConf),
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	webtransport.ConfigureHTTP3Server(h3Server)

	wtServer := &webtransport.Server{
		H3:          h3Server,
		CheckOrigin: func(*http.Request) bool { return true }, // T1: без CORS-обмежень
	}

	mux := http.NewServeMux()
	h3Server.Handler = mux
	mux.HandleFunc("/wt", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != authToken() {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sess, err := wtServer.Upgrade(w, r)
		if err != nil {
			log.Printf("hub-wt: webtransport upgrade failed: %v", err)
			return
		}
		log.Printf("hub-wt: viewer session established")
		go serveViewer(sess, relay, agentCtrl)
	})

	log.Printf("hub-wt: webtransport listening on %s%s", wtListenAddr, "/wt")
	return wtServer.ListenAndServe()
}

func serveViewer(sess *webtransport.Session, relay *hub.RelayBuf, agentCtrl *agentControl) {
	// стартуємо глядача зі свіжого IDR, а не з бэклогу
	relay.TrimToLatestKeyframe()
	if relay.Empty() {
		// Після трима в буфері немає жодного keyframe (агент ще не встиг
		// надіслати IDR у поточному epoch) — просимо агента про свіжий,
		// щоб не змушувати глядача чекати на природний цикл GOP.
		log.Printf("hub-wt: viewer join, no keyframe buffered, requesting one from agent")
		agentCtrl.RequestKeyframe()
	}

	go serveViewerControl(sess, agentCtrl)

	str, err := sess.OpenUniStreamSync(sess.Context())
	if err != nil {
		log.Printf("hub-wt: viewer uni-stream open failed: %v", err)
		return
	}
	defer str.Close()

	for {
		f, ok := relay.Pop()
		if !ok {
			return
		}
		buf, err := f.Marshal()
		if err != nil {
			log.Printf("hub-wt: marshal frame seq=%d failed: %v", f.FrameSeq, err)
			continue
		}
		if _, err := str.Write(buf); err != nil {
			log.Printf("hub-wt: viewer write failed, dropping session: %v", err)
			return
		}
		logNDJSON("viewer", int64(f.FrameSeq), monoMs())
	}
}

// serveViewerControl приймає ДРУГИЙ, bidi control-стрім, який відкриває сам
// браузер (viewer-wt.html/wt-worker.js) — окремо від першого uni-стріму з
// відео, яким hub ретранслює envelope-кадри. Тут читаються hello,
// decoder_ready, keyframe_request, heartbeat від глядача; keyframe_request
// транслюється агенту через agentCtrl (з дебаунсом).
func serveViewerControl(sess *webtransport.Session, agentCtrl *agentControl) {
	ctx := sess.Context()
	str, err := sess.AcceptStream(ctx)
	if err != nil {
		log.Printf("hub-wt: viewer control stream accept failed: %v", err)
		return
	}
	br := bufio.NewReader(str)
	for {
		m, err := control.ReadKnown(br, nil)
		if err != nil {
			log.Printf("hub-wt: viewer control stream done: %v", err)
			return
		}
		switch m.Type {
		case control.TypeHello:
			log.Printf("hub-wt: viewer control hello seq=%d", m.Seq)
		case control.TypeDecoderReady:
			log.Printf("hub-wt: viewer decoder_ready seq=%d", m.Seq)
		case control.TypeKeyframeRequest:
			log.Printf("hub-wt: viewer keyframe_request seq=%d, relaying to agent", m.Seq)
			agentCtrl.RequestKeyframe()
		case control.TypeHeartbeat:
			log.Printf("hub-wt: viewer heartbeat seq=%d", m.Seq)
		default:
			log.Printf("hub-wt: viewer control msg type=%s seq=%d", m.Type, m.Seq)
		}
	}
}
