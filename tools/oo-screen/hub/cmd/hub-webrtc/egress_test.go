package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

// egressPayload — пакет із номером у перших 4 байтах і заповненням за
// номером: на прийомі видно і порядок, і цілісність (GSO ріже рівно по
// межах сегментів, не зсуваючи байти).
func egressPayload(i, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint32(b, uint32(i))
	for k := 4; k < size; k++ {
		b[k] = byte(i + k)
	}
	return b
}

func listenLoop(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadBuffer(4 << 20)
	t.Cleanup(func() { c.Close() })
	return c
}

// TestEgressConnOrderAndIntegrity — серії однакових пакетів (GSO), коротший
// хвіст серії, поодинокі пакети різного розміру й дві адреси впереміш: кожен
// приймач має отримати свої пакети рівно в порядку запису й байт-у-байт.
// Ганяється і з GSO (де ядро вміє), і без нього.
func TestEgressConnOrderAndIntegrity(t *testing.T) {
	for _, mode := range []struct {
		name      string
		gso, mmsg bool
	}{{"gso+mmsg", true, true}, {"gso", true, false}, {"mmsg", false, true}, {"sendto", false, false}} {
		gso, mmsg := mode.gso, mode.mmsg
		t.Run(mode.name, func(t *testing.T) {
			src, err := net.ListenUDP("udp", &net.UDPAddr{})
			if err != nil {
				t.Fatal(err)
			}
			e := newEgressConn(src)
			defer e.Close()
			if !gso {
				e.gso.Store(false)
			} else if !e.gso.Load() {
				t.Skip("UDP GSO недоступний на цьому ядрі/ОС")
			}
			if !mmsg {
				e.mmsg.Store(false)
			} else if !e.mmsg.Load() {
				t.Skip("sendmmsg недоступний на цій ОС")
			}
			rx := []*net.UDPConn{listenLoop(t), listenLoop(t)}
			var want [2][][]byte
			sizes := func(i int) int {
				switch {
				case i%50 == 49:
					return 300 // короткий хвіст серії
				case i%17 == 0:
					return 700 // поодинокий інший розмір посеред серії
				default:
					return 1216
				}
			}
			const n = 400
			for i := 0; i < n; i++ {
				d := i % 2
				if i%40 < 20 { // ділянки «одна адреса поспіль» — довгі серії
					d = 0
				}
				p := egressPayload(i, sizes(i))
				want[d] = append(want[d], p)
				to := rx[d].LocalAddr().(*net.UDPAddr).AddrPort()
				if _, err := e.WriteToAddrPort(p, to); err != nil {
					t.Fatal(err)
				}
			}
			for d, c := range rx {
				buf := make([]byte, 2048)
				for k, w := range want[d] {
					_ = c.SetReadDeadline(time.Now().Add(eventGuard)) // запобіжник, не вікно
					m, _, err := c.ReadFromUDP(buf)
					if err != nil {
						t.Fatalf("приймач %d: пакет %d з %d: %v", d, k, len(want[d]), err)
					}
					if !bytes.Equal(buf[:m], w) {
						t.Fatalf("приймач %d, пакет %d: отримано #%d (%d Б), чекали #%d (%d Б)", d, k,
							binary.BigEndian.Uint32(buf), m, binary.BigEndian.Uint32(w), len(w))
					}
				}
			}
		})
	}
}

// TestEgressConnManyDestinations — R4: пачка з пакетами на десятки різних
// адрес (типова картина fan-out на сотні глядачів) іде через sendmmsg і з
// IPv4-, і з dual-stack сокета; кожен приймач отримує своє рівно в порядку.
func TestEgressConnManyDestinations(t *testing.T) {
	for _, network := range []string{"udp4", "udp"} {
		t.Run(network, func(t *testing.T) {
			src, err := net.ListenUDP(network, &net.UDPAddr{})
			if err != nil {
				t.Fatal(err)
			}
			e := newEgressConn(src)
			defer e.Close()
			const dests, per = 48, 6
			rx := make([]*net.UDPConn, dests)
			for i := range rx {
				rx[i] = listenLoop(t)
			}
			for k := 0; k < per; k++ {
				for d := range rx {
					p := egressPayload(k*dests+d, 400+d*10)
					if _, err := e.WriteToAddrPort(p, rx[d].LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
						t.Fatal(err)
					}
				}
			}
			buf := make([]byte, 2048)
			for d, c := range rx {
				for k := 0; k < per; k++ {
					_ = c.SetReadDeadline(time.Now().Add(eventGuard)) // запобіжник, не вікно
					m, _, err := c.ReadFromUDP(buf)
					if err != nil {
						t.Fatalf("приймач %d, пакет %d: %v", d, k, err)
					}
					if w := egressPayload(k*dests+d, 400+d*10); !bytes.Equal(buf[:m], w) {
						t.Fatalf("приймач %d, пакет %d: отримано #%d", d, k, binary.BigEndian.Uint32(buf))
					}
				}
			}
			if e.errs.Load() != 0 {
				t.Fatalf("помилок запису: %d", e.errs.Load())
			}
		})
	}
}

// TestEgressConnWriteToUDPAddr — шлях net.PacketConn.WriteTo (pion кличе його,
// коли AddrPort-шлях недоступний) веде в ту саму чергу.
func TestEgressConnWriteToUDPAddr(t *testing.T) {
	src, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEgressConn(src)
	defer e.Close()
	rx := listenLoop(t)
	if n, err := e.WriteTo([]byte("hello"), rx.LocalAddr()); err != nil || n != 5 {
		t.Fatalf("WriteTo = %d, %v", n, err)
	}
	buf := make([]byte, 64)
	_ = rx.SetReadDeadline(time.Now().Add(3 * time.Second))
	m, _, err := rx.ReadFromUDP(buf)
	if err != nil || string(buf[:m]) != "hello" {
		t.Fatalf("прийнято %q, %v", buf[:m], err)
	}
}

// TestEgressConnClose — після Close запис повертає net.ErrClosed, а не
// зависає на черзі й не панікує; Close ідемпотентний.
func TestEgressConnClose(t *testing.T) {
	src, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	e := newEgressConn(src)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	_ = e.Close()
	to := netip.MustParseAddrPort("127.0.0.1:9")
	if _, err := e.WriteToAddrPort([]byte{1}, to); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("запис після Close: %v, want net.ErrClosed", err)
	}
	if _, _, err := e.ReadFrom(make([]byte, 8)); err == nil {
		t.Fatal("читання після Close мало б повернути помилку")
	}
}

// BenchmarkEgressFanout — R4: писар під fan-out-ом на 200 адрес (по пакету
// 1216 Б на кожну по колу — так виглядає пачка при сотнях глядачів, GSO тут
// не зливає нічого). sendmmsg проти sendto по датаграмі; ns/op — на пакет,
// разом із доставкою в сокети-приймачі (loopback; приймачі не читаються,
// надлишок ядро викидає).
func BenchmarkEgressFanout(b *testing.B) {
	for _, mmsg := range []bool{false, true} {
		name := "sendto"
		if mmsg {
			name = "sendmmsg"
		}
		b.Run(name, func(b *testing.B) {
			src, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				b.Fatal(err)
			}
			e := newEgressConn(src)
			defer e.Close()
			if !mmsg {
				e.mmsg.Store(false)
			} else if !e.mmsg.Load() {
				b.Skip("sendmmsg недоступний")
			}
			const dests = 200
			to := make([]netip.AddrPort, dests)
			for i := range to {
				c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					b.Fatal(err)
				}
				defer c.Close()
				to[i] = c.LocalAddr().(*net.UDPAddr).AddrPort()
			}
			p := egressPayload(1, 1216)
			b.SetBytes(int64(len(p)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := e.WriteToAddrPort(p, to[i%dests]); err != nil {
					b.Fatal(err)
				}
			}
			// Дочекатись, поки писар віддасть чергу ядру.
			for len(e.q) > 0 {
				time.Sleep(50 * time.Microsecond)
			}
		})
	}
}
