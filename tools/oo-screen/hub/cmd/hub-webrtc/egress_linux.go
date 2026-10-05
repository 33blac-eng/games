//go:build linux

package main

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// gsoSupported — чи приймає сокет UDP_SEGMENT (Linux ≥ 4.18). Перевірка —
// getsockopt; OO_SCREEN_UDP_GSO=0 вимикає GSO вручну (на випадок драйвера,
// що каже «так», а пакети ламає; помилку відправки writer і сам ловить).
func gsoSupported(c *net.UDPConn) bool {
	if os.Getenv("OO_SCREEN_UDP_GSO") == "0" {
		return false
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	_ = rc.Control(func(fd uintptr) {
		_, err := unix.GetsockoptInt(int(fd), unix.IPPROTO_UDP, unix.UDP_SEGMENT)
		ok = err == nil
	})
	return ok
}

// writeGSO шле b однією датаграмою, яку ядро ріже на сегменти по seg байт
// (останній може бути коротшим).
func writeGSO(c *net.UDPConn, b []byte, seg int, to netip.AddrPort) error {
	var raw [64]byte
	oob := raw[:unix.CmsgSpace(2)]
	h := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
	h.Level = unix.IPPROTO_UDP
	h.Type = unix.UDP_SEGMENT
	h.SetLen(unix.CmsgLen(2))
	binary.NativeEndian.PutUint16(oob[unix.CmsgLen(0):], uint16(seg))
	_, _, err := c.WriteMsgUDPAddrPort(b, oob, to)
	return err
}
