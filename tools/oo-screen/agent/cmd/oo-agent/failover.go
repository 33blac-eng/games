package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// O2: резервний хаб (standby) з перевіркою здоров'я перед перемиканням.
//
// Типово ВИМКНЕНО: без -hub-standby селектор має рівно одну адресу і завжди
// повертає її — поведінка агента як до O2.
//
// Правило: після failAfter поспіль невдалих dial до поточного хаба агент
// опитує GET /healthz кандидатів (у порядку списку, починаючи з наступного)
// і переходить на перший, що відповів 200 з "ok":true. Якщо здорових нема —
// лишається на поточному (без пінг-понгу між двома мертвими хабами).
// Автоматичного повернення на основний, поки резервний живий, НЕМА навмисно:
// це зайвий обрив сесій. Повернення — при наступному збої резервного.

const (
	defaultFailoverAfter = 3
	healthzTimeout       = 3 * time.Second
)

type healthFunc func(ctx context.Context, offerURL string) error

type hubSelector struct {
	addrs     []string
	cur       int
	fails     int
	failAfter int
	health    healthFunc
}

// newHubSelector: primary + standby (через кому). Порожній standby = OFF.
func newHubSelector(primary, standby string, failAfter int, h healthFunc) *hubSelector {
	s := &hubSelector{addrs: []string{primary}, failAfter: failAfter, health: h}
	for _, a := range strings.Split(standby, ",") {
		if a = strings.TrimSpace(a); a != "" && a != primary {
			s.addrs = append(s.addrs, a)
		}
	}
	if s.failAfter < 1 {
		s.failAfter = defaultFailoverAfter
	}
	return s
}

func (s *hubSelector) enabled() bool   { return len(s.addrs) > 1 }
func (s *hubSelector) current() string { return s.addrs[s.cur] }

// ok — dial вдався: лічильник збоїв з нуля.
func (s *hubSelector) ok() { s.fails = 0 }

// failed — dial до current() не вдався. Повертає true, якщо переключились.
func (s *hubSelector) failed(ctx context.Context) bool {
	if !s.enabled() {
		return false
	}
	s.fails++
	if s.fails < s.failAfter {
		return false
	}
	for i := 1; i < len(s.addrs); i++ {
		j := (s.cur + i) % len(s.addrs)
		if err := s.health(ctx, s.addrs[j]); err != nil {
			log.Printf("oo-agent: failover: %s нездоровий: %v", s.addrs[j], err)
			continue
		}
		log.Printf("oo-agent: failover: %s -> %s після %d збоїв", s.current(), s.addrs[j], s.fails)
		s.cur, s.fails = j, 0
		return true
	}
	s.fails = 0 // наступна перевірка — знову через failAfter спроб
	return false
}

// healthzURL: http://h:p/offer/agent -> http://h:p/healthz.
func healthzURL(offerURL string) (string, error) {
	u, err := url.Parse(offerURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("невалідна адреса хаба %q", offerURL)
	}
	u.Path, u.RawQuery, u.Fragment = "/healthz", "", ""
	return u.String(), nil
}

// httpHealth — реальна перевірка: 200 і JSON {"ok":true}.
func httpHealth(ctx context.Context, offerURL string) error {
	hu, err := healthzURL(offerURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hu, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz %d", resp.StatusCode)
	}
	var b struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&b); err != nil {
		return fmt.Errorf("healthz json: %w", err)
	}
	if !b.OK {
		return fmt.Errorf("healthz ok=false")
	}
	return nil
}
