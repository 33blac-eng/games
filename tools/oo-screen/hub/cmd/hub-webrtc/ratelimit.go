// ratelimit.go — SEC #21: глобальні межі публічного сигналінгу.
//
//   - OO_SCREEN_MAX_NODES (дефолт 500) — стеля нод у реєстрі: нова нода понад
//     неї не створюється (503), наявні працюють як раніше.
//   - per-IP token bucket на /offer/viewer і /offer/agent: кожен viewer-offer =
//     виклик ERP, кожен agent-offer = PeerConnection. OO_SCREEN_OFFER_RATE
//     (запитів/с, дефолт 1) і OO_SCREEN_OFFER_BURST (дефолт 10); RATE=0 вимикає.
//   - X-Forwarded-For враховується ЛИШЕ від проксі зі списку
//     OO_SCREEN_TRUSTED_PROXIES (IP або CIDR через кому), інакше будь-хто
//     обходив би ліміт підробленим заголовком.
package main

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// maxNodes — стеля нод у реєстрі.
var maxNodes = envPositiveInt("OO_SCREEN_MAX_NODES", 500)

func envPositiveInt(k string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(k))); err == nil && v > 0 {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(k)), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

// ipLimiter — token bucket на IP клієнта з прибиранням тих, хто давно мовчить.
type ipLimiter struct {
	mu      sync.Mutex
	r       rate.Limit
	burst   int
	m       map[string]*ipEntry
	lastGC  time.Time
	trusted []*net.IPNet
}

type ipEntry struct {
	lim  *rate.Limiter
	seen time.Time
}

const ipIdleTTL = 10 * time.Minute

func newIPLimiter(perSec float64, burst int, trusted string) *ipLimiter {
	return &ipLimiter{r: rate.Limit(perSec), burst: burst, m: make(map[string]*ipEntry), trusted: parseTrusted(trusted)}
}

func parseTrusted(s string) []*net.IPNet {
	var out []*net.IPNet
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			if ip := net.ParseIP(f); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				f += "/" + strconv.Itoa(bits)
			}
		}
		if _, n, err := net.ParseCIDR(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (l *ipLimiter) isTrusted(ip net.IP) bool {
	for _, n := range l.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP — адреса клієнта. XFF читається справа наліво лише поки попередній
// хоп — довірений проксі; перший недовірений і є клієнтом.
func (l *ipLimiter) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !l.isTrusted(peer) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break
		}
		if !l.isTrusted(ip) {
			return ip.String()
		}
		host = ip.String()
	}
	return host
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	if l.r == 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) > time.Minute {
		for k, e := range l.m {
			if now.Sub(e.seen) > ipIdleTTL {
				delete(l.m, k)
			}
		}
		l.lastGC = now
	}
	e := l.m[ip]
	if e == nil {
		e = &ipEntry{lim: rate.NewLimiter(l.r, l.burst)}
		l.m[ip] = e
	}
	e.seen = now
	return e.lim.AllowN(now, 1)
}

// offerLimiter — спільний на /offer/viewer і /offer/agent.
var offerLimiter = newIPLimiter(
	envFloat("OO_SCREEN_OFFER_RATE", 1),
	envPositiveInt("OO_SCREEN_OFFER_BURST", 10),
	trustedProxiesEnv(),
)

// defaultTrustedProxies — довіра X-Forwarded-For від loopback, коли
// OO_SCREEN_TRUSTED_PROXIES не задано зовсім. Прод-хаб стоїть за nginx на тому
// ж хості: без цього ВСІ /offer/* мали б RemoteAddr=127.0.0.1 і ділили б один
// кошик (1/с, сплеск 10) — глядачі й агенти всього парку разом. Loopback-з'єднання
// може відкрити лише локальний процес, тож довіра безпечна; без XFF клієнтом
// лишається сам loopback (як і без довіри). Явне порожнє значення — без довіри.
const defaultTrustedProxies = "127.0.0.0/8,::1"

func trustedProxiesEnv() string {
	if v, ok := os.LookupEnv("OO_SCREEN_TRUSTED_PROXIES"); ok {
		return v
	}
	return defaultTrustedProxies
}

// rateLimited загортає хендлер у per-IP ліміт (OPTIONS не рахуються).
func rateLimited(l *ipLimiter, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodOptions && !l.allow(l.clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		h(w, r)
	}
}
