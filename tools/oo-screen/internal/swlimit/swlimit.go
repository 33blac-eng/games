// Package swlimit — автоліміти для СОФТВЕРНОГО H.264-енкодера на слабкому CPU
// (ТЗ P8, RESEARCH-leaders.md: ПК без GPU-енкодера).
//
// Політика чиста: на вході — скільки реально тривало кодування кадру, кількість
// ядер і момент виміру; на виході — стеля FPS і (порадою) крок роздільності.
// Жодних build-тегів, жодного WinAPI — тестується на будь-якій ОС.
//
// Порядок поступок — спершу FPS, потім роздільність: для робочого столу
// чіткість тексту важить більше за плавність (RESEARCH gap #4). Відновлення —
// у зворотному порядку: спершу роздільність, потім FPS.
//
// Гістерезис: униз — коли згладжене завантаження вище High утримується
// DownHold; угору — коли ПРОГНОЗОВАНЕ завантаження на наступному (важчому)
// кроці нижче Low утримується UpHold. Прогноз пропорційний кадрам/с і площі
// кадру, тож політика не «пиляє» між двома сусідніми кроками.
package swlimit

import (
	"fmt"
	"time"
)

// Scale — крок роздільності як дріб від рідної (Num/Den).
type Scale struct{ Num, Den int }

func (s Scale) area() float64 { f := float64(s.Num) / float64(s.Den); return f * f }

// Config — пороги політики. Нулі замінюються дефолтами в New.
type Config struct {
	MaxFPS   int           // запитана частота; верхній щабель драбини
	MinFPS   int           // нижче не опускаємось (дефолт 10)
	FPSSteps []int         // кандидати драбини FPS (спадна), обрізаються під MaxFPS/MinFPS
	Scales   []Scale       // драбина роздільності (спадна); дефолт {1/1, 3/4, 2/3}
	Cores    int           // runtime.NumCPU()
	High     float64       // «не тягне»: частка інтервалу кадру, зайнята кодуванням
	Low      float64       // «є запас»: поріг для прогнозу наступного кроку
	DownHold time.Duration // скільки High має триматись до поступки (дефолт 2с)
	UpHold   time.Duration // скільки запас має триматись до відновлення (дефолт 10с)
	Alpha    float64       // EWMA-коефіцієнт (дефолт 0.2)
}

// Decision — результат Observe.
type Decision struct {
	FPS     int
	Scale   Scale
	Changed bool
	Reason  string
}

// Policy — стан. Не потокобезпечна: кличеться з одного кадрового циклу.
type Policy struct {
	c         Config
	fi, si    int // індекси у FPSSteps / Scales
	ewma      float64
	have      bool
	overFrom  time.Time
	underFrom time.Time
}

// budgetShare — яку частку інтервалу кадру дозволено віддати енкодеру. На
// 1-2 ядрах синхронний софт-MFT ділить CPU з застосунками користувача і з
// нашим же захопленням/readback, тож запас мусить бути більшим.
func budgetShare(cores int) float64 {
	switch {
	case cores <= 2:
		return 0.5
	case cores <= 4:
		return 0.7
	default:
		return 0.85
	}
}

// New будує політику з дефолтами.
func New(c Config) *Policy {
	if c.MaxFPS <= 0 {
		c.MaxFPS = 30
	}
	if c.MinFPS <= 0 {
		c.MinFPS = 10
	}
	if c.MinFPS > c.MaxFPS {
		c.MinFPS = c.MaxFPS
	}
	src := c.FPSSteps
	if len(src) == 0 {
		src = []int{60, 50, 30, 24, 20, 15, 12, 10}
	}
	steps := []int{c.MaxFPS}
	for _, f := range src {
		if f >= c.MinFPS && f < steps[len(steps)-1] {
			steps = append(steps, f)
		}
	}
	if steps[len(steps)-1] != c.MinFPS {
		steps = append(steps, c.MinFPS)
	}
	c.FPSSteps = steps
	if len(c.Scales) == 0 {
		c.Scales = []Scale{{1, 1}, {3, 4}, {2, 3}}
	}
	if c.Cores <= 0 {
		c.Cores = 1
	}
	if c.High <= 0 {
		c.High = budgetShare(c.Cores)
	}
	if c.Low <= 0 {
		c.Low = c.High * 0.75
	}
	if c.DownHold <= 0 {
		c.DownHold = 2 * time.Second
	}
	if c.UpHold <= 0 {
		c.UpHold = 10 * time.Second
	}
	if c.Alpha <= 0 || c.Alpha > 1 {
		c.Alpha = 0.2
	}
	return &Policy{c: c}
}

// FPS — поточна стеля.
func (p *Policy) FPS() int { return p.c.FPSSteps[p.fi] }

// Scale — поточний крок роздільності.
func (p *Policy) Scale() Scale { return p.c.Scales[p.si] }

// Load — згладжене завантаження (частка інтервалу кадру на поточній стелі).
func (p *Policy) Load() float64 { return p.ewma * float64(p.FPS()) }

func (p *Policy) decision(changed bool, why string) Decision {
	return Decision{FPS: p.FPS(), Scale: p.Scale(), Changed: changed, Reason: why}
}

// Observe приймає тривалість кодування одного кадру (стінний час Encode) і
// момент виміру. Changed=true — крок змінився, Reason пояснює чому.
func (p *Policy) Observe(enc time.Duration, now time.Time) Decision {
	sec := enc.Seconds()
	if sec < 0 {
		sec = 0
	}
	if !p.have {
		p.ewma, p.have = sec, true
	} else {
		p.ewma += p.c.Alpha * (sec - p.ewma)
	}
	load := p.Load()

	if load > p.c.High {
		p.underFrom = time.Time{}
		if p.overFrom.IsZero() {
			p.overFrom = now
		}
		if now.Sub(p.overFrom) < p.c.DownHold {
			return p.decision(false, "")
		}
		p.overFrom = now // наступна поступка — знову не раніше DownHold
		switch {
		case p.fi < len(p.c.FPSSteps)-1:
			p.fi++
			return p.decision(true, fmt.Sprintf("down fps: load %.2f > %.2f", load, p.c.High))
		case p.si < len(p.c.Scales)-1:
			p.si++
			// Менше пікселів — EWMA масштабуємо одразу, щоб не чекати, поки
			// воно «забуде» важчий крок.
			p.ewma *= p.c.Scales[p.si].area() / p.c.Scales[p.si-1].area()
			return p.decision(true, fmt.Sprintf("down scale: load %.2f > %.2f", load, p.c.High))
		}
		return p.decision(false, "")
	}
	p.overFrom = time.Time{}

	// Прогноз завантаження на наступному важчому кроці.
	var next float64
	switch {
	case p.si > 0:
		next = load * p.c.Scales[p.si-1].area() / p.Scale().area()
	case p.fi > 0:
		next = load * float64(p.c.FPSSteps[p.fi-1]) / float64(p.FPS())
	default:
		p.underFrom = time.Time{}
		return p.decision(false, "")
	}
	if next >= p.c.Low {
		p.underFrom = time.Time{}
		return p.decision(false, "")
	}
	if p.underFrom.IsZero() {
		p.underFrom = now
	}
	if now.Sub(p.underFrom) < p.c.UpHold {
		return p.decision(false, "")
	}
	p.underFrom = now
	if p.si > 0 {
		p.ewma *= p.c.Scales[p.si-1].area() / p.Scale().area()
		p.si--
		return p.decision(true, fmt.Sprintf("up scale: next %.2f < %.2f", next, p.c.Low))
	}
	p.fi--
	return p.decision(true, fmt.Sprintf("up fps: next %.2f < %.2f", next, p.c.Low))
}

// FrameGap — мінімальний проміжок між кодованими кадрами для поточної стелі
// (0 = не притискати, коли стеля = MaxFPS).
func (p *Policy) FrameGap() time.Duration {
	if p.fi == 0 {
		return 0
	}
	return time.Second / time.Duration(p.FPS())
}
