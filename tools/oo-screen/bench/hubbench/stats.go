package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// procSample — знімок процесу з /proc. CPU у тіках (USER_HZ=100).
type procSample struct {
	at      time.Time
	cpuTick uint64
	rssKB   uint64
	threads int
}

func readProc(pid int) (procSample, error) {
	s := procSample{at: time.Now()}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return s, err
	}
	ut, st, err := parseStatCPU(stat)
	if err != nil {
		return s, err
	}
	s.cpuTick = ut + st
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return s, err
	}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "VmRSS:":
			s.rssKB, _ = strconv.ParseUint(f[1], 10, 64)
		case "Threads:":
			s.threads, _ = strconv.Atoi(f[1])
		}
	}
	return s, nil
}

// parseStatCPU — utime і stime (поля 14 і 15) з /proc/PID/stat. comm може
// містити пробіли й дужки, тож рахуємо від ОСТАННЬОЇ ')'.
func parseStatCPU(stat []byte) (uint64, uint64, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("bad stat")
	}
	f := strings.Fields(string(stat[i+1:]))
	// після ')' поле 3 (state) має індекс 0 => utime(14) = індекс 11.
	if len(f) < 13 {
		return 0, 0, fmt.Errorf("short stat")
	}
	ut, err1 := strconv.ParseUint(f[11], 10, 64)
	st, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("bad cpu fields")
	}
	return ut, st, nil
}

// cpuPct — CPU% між двома знімками (100% = одне ядро).
func cpuPct(a, b procSample) float64 {
	dt := b.at.Sub(a.at).Seconds()
	if dt <= 0 {
		return 0
	}
	return float64(b.cpuTick-a.cpuTick) / 100 / dt * 100
}

// goroutines — «goroutine profile: total N» з pprof хаба.
func goroutines(pprofAddr string) int {
	if pprofAddr == "" {
		return -1
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + pprofAddr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return parseGoroutineTotal(head)
}

// heapInuseMB — runtime.MemStats.HeapInuse хаба з /debug/pprof/heap?debug=1
// (рядок "# HeapInuse = N" наприкінці). RSS сам по собі не відрізняє витік
// від памʼяті, яку рантайм ще не повернув ОС.
func heapInuseMB(pprofAddr string) float64 {
	if pprofAddr == "" {
		return -1
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("http://" + pprofAddr + "/debug/pprof/heap?gc=1&debug=1")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return parseHeapInuseMB(b)
}

func parseHeapInuseMB(b []byte) float64 {
	const p = "# HeapInuse = "
	i := bytes.Index(b, []byte(p))
	if i < 0 {
		return -1
	}
	rest := b[i+len(p):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	n, err := strconv.ParseUint(string(rest[:j]), 10, 64)
	if err != nil {
		return -1
	}
	return float64(n) / (1 << 20)
}

func parseGoroutineTotal(b []byte) int {
	const p = "goroutine profile: total "
	i := bytes.Index(b, []byte(p))
	if i < 0 {
		return -1
	}
	rest := b[i+len(p):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	n, err := strconv.Atoi(string(rest[:j]))
	if err != nil {
		return -1
	}
	return n
}

// percentile по відсортованій копії; p у [0,100].
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	idx := int(p / 100 * float64(len(s)-1))
	return s[idx]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	t := 0.0
	for _, x := range xs {
		t += x
	}
	return t / float64(len(xs))
}
