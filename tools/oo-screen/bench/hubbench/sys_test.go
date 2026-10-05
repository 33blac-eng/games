package main

import "testing"

func TestParseHeapInuse(t *testing.T) {
	b := []byte("heap profile: ...\n# HeapAlloc = 1\n# HeapInuse = 3145728\n# Stack = 2\n")
	if got := parseHeapInuseMB(b); got != 3 {
		t.Fatalf("got %v", got)
	}
	if parseHeapInuseMB([]byte("nope")) != -1 {
		t.Fatal("missing must be -1")
	}
}

func TestParseSys(t *testing.T) {
	a := parseProcStatCPU([]byte("cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 1 2 3\n"))
	b := parseProcStatCPU([]byte("cpu  200 0 200 750 150 0 0 0 0 0\n"))
	if got := b.idlePct(a); got < 33.3 || got > 33.4 {
		t.Fatalf("idle %v", got)
	}
	snmp := "Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors\nUdp: 10 0 7 20 7 0\n"
	if parseRcvbufErrors([]byte(snmp)) != 7 {
		t.Fatal("rcvbuf")
	}
}
