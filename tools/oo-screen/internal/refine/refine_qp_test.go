package refine

import (
	"testing"
	"time"
)

// IDR від rate control на нерухомому екрані (періодичний GOP, новий
// глядач) без AfterKeyframe лишає екран мильним до наступного руху.
func TestKeyframeOnStillScreen(t *testing.T) {
	t0 := time.Unix(0, 0)
	for _, after := range []bool{false, true} {
		s := New(Config{MinGap: 33 * time.Millisecond, AfterKeyframe: after})
		s.Motion(t0)
		now := t0.Add(DefaultIdle)
		for _, want := range []int{22, 18} {
			qp, ok := s.Due(now)
			if !ok || qp != want {
				t.Fatalf("refine %d %v, хочемо %d", qp, ok, want)
			}
			s.Sent(now, 1000, 12_000_000)
			s.Coded(now, Frame{Refine: qp, QP: qp})
			now = now.Add(33 * time.Millisecond)
		}
		if !s.Complete() {
			t.Fatal("refine не завершено")
		}
		// Через 5 с — IDR на keepalive-кадрі.
		idr := now.Add(5 * time.Second)
		s.Coded(idr, Frame{Key: true, QP: 30})
		_, ok := s.Due(idr.Add(DefaultIdle))
		if ok != after {
			t.Fatalf("AfterKeyframe=%v: refine після IDR = %v", after, ok)
		}
		if after {
			if s.Complete() {
				t.Fatal("після IDR refine ще не дошліфував")
			}
			if w := s.Wait(idr, time.Second); w != DefaultIdle {
				t.Fatalf("wait після IDR %v", w)
			}
		}
	}
}

// Refine-кадр, закодований як IDR (ForceIDR збігся з refine), сам себе
// заново не заводить.
func TestRefineKeyframeDoesNotRearm(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{AfterKeyframe: true})
	s.Motion(t0)
	now := t0.Add(DefaultIdle)
	for i := 0; i < 2; i++ {
		qp, ok := s.Due(now)
		if !ok {
			t.Fatal("refine не настав")
		}
		s.Sent(now, 100, 0)
		s.Coded(now, Frame{Key: i == 1, Refine: qp})
		now = now.Add(time.Second)
	}
	if _, ok := s.Due(now.Add(time.Hour)); ok || !s.Complete() {
		t.Fatal("refine-IDR завів refine знову")
	}
	if s.WorstQP() != 18 {
		t.Fatalf("worst %d", s.WorstQP())
	}
}

func TestQPAwareSkipsUselessSteps(t *testing.T) {
	t0 := time.Unix(0, 0)
	cases := []struct {
		name  string
		seq   []Frame // після Motion
		steps []int   // очікувані refine-кадри
	}{
		{"мильний рух", []Frame{{Key: true, QP: 24}, {Motion: true, QP: 34}}, []int{22, 18}},
		{"рух уже кращий за 22", []Frame{{Key: true, QP: 20}, {Motion: true, QP: 19}}, []int{18}},
		{"рух кращий за обидва", []Frame{{Key: true, QP: 16}, {Motion: true, QP: 15}}, nil},
		// Дрібний якісний P-кадр (набір) не робить кращим решту екрана після
		// мильного прокручування: найгірший лишається 35.
		{"дрібна зміна після скролу", []Frame{{Key: true, QP: 20}, {Motion: true, QP: 35}, {Motion: true, QP: 17}}, []int{22, 18}},
		// QP невідомий — поводимось як без прапорця.
		{"QP невідомий", []Frame{{Key: true, QP: 16}, {Motion: true}}, []int{22, 18}},
		{"keepalive не міняє", []Frame{{Key: true, QP: 16}, {}, {QP: 40}}, nil},
	}
	for _, c := range cases {
		s := New(Config{QPAware: true})
		s.Motion(t0)
		for _, f := range c.seq {
			s.Coded(t0, f)
		}
		now := t0.Add(DefaultIdle)
		var got []int
		for i := 0; i < 5; i++ {
			qp, ok := s.Due(now)
			if !ok {
				break
			}
			got = append(got, qp)
			s.Sent(now, 100, 0)
			s.Coded(now, Frame{Refine: qp, QP: qp})
			now = now.Add(time.Second)
		}
		if len(got) != len(c.steps) {
			t.Fatalf("%s: refine %v, хочемо %v", c.name, got, c.steps)
		}
		for i := range got {
			if got[i] != c.steps[i] {
				t.Fatalf("%s: refine %v, хочемо %v", c.name, got, c.steps)
			}
		}
		if !s.Complete() {
			t.Fatalf("%s: не Complete після всіх кроків (тайли чекали б вічно)", c.name)
		}
		if _, ok := s.Due(now.Add(time.Hour)); ok {
			t.Fatalf("%s: зайвий refine", c.name)
		}
	}
}

// Без QPAware Coded не змінює розклад refine (стара поведінка).
func TestCodedWithoutFlagsKeepsSchedule(t *testing.T) {
	t0 := time.Unix(0, 0)
	s := New(Config{})
	s.Motion(t0)
	s.Coded(t0, Frame{Key: true, QP: 10})
	now := t0.Add(DefaultIdle)
	for _, want := range []int{22, 18} {
		qp, ok := s.Due(now)
		if !ok || qp != want {
			t.Fatalf("%d %v, хочемо %d", qp, ok, want)
		}
		s.Sent(now, 100, 0)
		now = now.Add(time.Second)
	}
	s.Coded(now, Frame{Key: true, QP: 30})
	if _, ok := s.Due(now.Add(time.Hour)); ok {
		t.Fatal("IDR завів refine без AfterKeyframe")
	}
}

// MFT проігнорував QP семпла: віримо QP з потоку.
func TestRefineUsesMeasuredQP(t *testing.T) {
	s := New(Config{QPAware: true})
	s.Coded(time.Unix(0, 0), Frame{Key: true, QP: 30})
	s.Coded(time.Unix(0, 0), Frame{Refine: 18, QP: 26})
	if s.WorstQP() != 26 {
		t.Fatalf("worst %d", s.WorstQP())
	}
}
