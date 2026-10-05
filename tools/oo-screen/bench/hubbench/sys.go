package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
)

// Машина спільна з іншими навантаженнями: кожен прогін фіксує, скільки CPU
// у всієї системи було вільним і скільки UDP-датаграм ядро викинуло через
// повний буфер приймача (будь-якого сокета на машині).

// sysCPU — агрегатний рядок "cpu" з /proc/stat.
type sysCPU struct{ idle, total uint64 }

func (b sysCPU) idlePct(a sysCPU) float64 {
	if b.total <= a.total {
		return 0
	}
	return float64(b.idle-a.idle) / float64(b.total-a.total) * 100
}

// readSys — /proc/stat і RcvbufErrors з /proc/net/snmp.
func readSys() (sysCPU, uint64) {
	var s sysCPU
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		s = parseProcStatCPU(b)
	}
	var rb uint64
	if b, err := os.ReadFile("/proc/net/snmp"); err == nil {
		rb = parseRcvbufErrors(b)
	}
	return s, rb
}

func parseProcStatCPU(b []byte) sysCPU {
	line, _, _ := bytes.Cut(b, []byte("\n"))
	f := strings.Fields(string(line))
	var s sysCPU
	if len(f) < 5 || f[0] != "cpu" {
		return s
	}
	for i, x := range f[1:] {
		v, _ := strconv.ParseUint(x, 10, 64)
		s.total += v
		if i == 3 || i == 4 { // idle, iowait
			s.idle += v
		}
	}
	return s
}

func parseRcvbufErrors(b []byte) uint64 {
	var hdr []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "Udp: ") {
			continue
		}
		f := strings.Fields(line)
		if hdr == nil {
			hdr = f
			continue
		}
		for i, h := range hdr {
			if h == "RcvbufErrors" && i < len(f) {
				v, _ := strconv.ParseUint(f[i], 10, 64)
				return v
			}
		}
	}
	return 0
}
