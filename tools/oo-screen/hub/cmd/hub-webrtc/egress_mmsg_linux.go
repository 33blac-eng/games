//go:build linux

package main

// R4: sendmmsg для писаря egressConn — один системний виклик на пачку
// датаграм різним глядачам замість sendto на кожну.

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// mmsghdr — struct mmsghdr ядра: msghdr + msg_len, вирівняне до слова.
type mmsghdr struct {
	hdr unix.Msghdr
	n   uint32
	_   [unsafe.Sizeof(uintptr(0)) - 4]byte
}

// mmsgMax — датаграм за один sendmmsg (UIO_MAXIOV = 1024; пачка ≤ egressBatch).
const mmsgMax = egressBatch

type mmsgState struct {
	rc   syscall.RawConn
	v6   bool // сокет AF_INET6 (dual-stack): IPv4 адреси — як ::ffff:a.b.c.d
	hdrs []mmsghdr
	iovs []unix.Iovec
	n4   []unix.RawSockaddrInet4
	n6   []unix.RawSockaddrInet6
	oob  [][unix.SizeofCmsghdr + 8]byte
}

// init — чи можна слати пачками; OO_SCREEN_SENDMMSG=0 вимикає (відкат і
// порівняльний замір).
func (m *mmsgState) init(c *net.UDPConn) bool {
	if os.Getenv("OO_SCREEN_SENDMMSG") == "0" {
		return false
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return false
	}
	var sa unix.Sockaddr
	if cerr := rc.Control(func(fd uintptr) { sa, err = unix.Getsockname(int(fd)) }); cerr != nil || err != nil {
		return false
	}
	switch sa.(type) {
	case *unix.SockaddrInet4:
	case *unix.SockaddrInet6:
		m.v6 = true
	default:
		return false
	}
	m.rc = rc
	m.hdrs = make([]mmsghdr, mmsgMax)
	m.iovs = make([]unix.Iovec, mmsgMax)
	m.n4 = make([]unix.RawSockaddrInet4, mmsgMax)
	m.n6 = make([]unix.RawSockaddrInet6, mmsgMax)
	m.oob = make([][unix.SizeofCmsghdr + 8]byte, mmsgMax)
	return true
}

// send шле префікс msgs одним sendmmsg; повертає, скільки датаграм прийнято.
// 0 і помилка — перша датаграма не пройшла (викликач шле її окремо: там і
// облік помилки, і відкат GSO). Адреса, яку не вміємо закодувати (IPv6 на
// IPv4-сокеті), обриває пачку перед собою.
func (m *mmsgState) send(msgs []egressMsg) (int, error) {
	cnt := 0
	for i := range msgs {
		if cnt == mmsgMax || !m.fill(cnt, &msgs[i]) {
			break
		}
		cnt++
	}
	if cnt == 0 {
		return 0, errors.New("адреса поза сімейством сокета")
	}
	var (
		sent  int
		errno syscall.Errno
	)
	err := m.rc.Write(func(fd uintptr) bool {
		r, _, e := unix.Syscall6(unix.SYS_SENDMMSG, fd, uintptr(unsafe.Pointer(&m.hdrs[0])), uintptr(cnt), 0, 0, 0)
		if e == unix.EAGAIN {
			return false // буфер сокета повний — чекаємо на poller
		}
		sent, errno = int(r), e
		return true
	})
	if err != nil {
		return 0, err
	}
	if errno != 0 {
		if errno == unix.ENOSYS {
			return 0, errMmsgUnsupported
		}
		return 0, errno
	}
	return sent, nil
}

func (m *mmsgState) fill(i int, msg *egressMsg) bool {
	h := &m.hdrs[i]
	*h = mmsghdr{}
	a := msg.to.Addr()
	if m.v6 {
		sa := &m.n6[i]
		*sa = unix.RawSockaddrInet6{Family: unix.AF_INET6, Addr: a.As16()}
		binary.BigEndian.PutUint16((*[2]byte)(unsafe.Pointer(&sa.Port))[:], msg.to.Port())
		if a.Is6() && !a.Is4In6() {
			sa.Scope_id = scopeID(a)
		}
		h.hdr.Name = (*byte)(unsafe.Pointer(sa))
		h.hdr.Namelen = unix.SizeofSockaddrInet6
	} else {
		a = a.Unmap()
		if !a.Is4() {
			return false
		}
		sa := &m.n4[i]
		*sa = unix.RawSockaddrInet4{Family: unix.AF_INET, Addr: a.As4()}
		binary.BigEndian.PutUint16((*[2]byte)(unsafe.Pointer(&sa.Port))[:], msg.to.Port())
		h.hdr.Name = (*byte)(unsafe.Pointer(sa))
		h.hdr.Namelen = unix.SizeofSockaddrInet4
	}
	iov := &m.iovs[i]
	iov.Base = unsafe.SliceData(msg.b)
	iov.SetLen(len(msg.b))
	h.hdr.Iov = iov
	h.hdr.SetIovlen(1)
	if msg.seg > 0 {
		oob := m.oob[i][:unix.CmsgSpace(2)]
		clear(oob)
		ch := (*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
		ch.Level = unix.IPPROTO_UDP
		ch.Type = unix.UDP_SEGMENT
		ch.SetLen(unix.CmsgLen(2))
		binary.NativeEndian.PutUint16(oob[unix.CmsgLen(0):], uint16(msg.seg))
		h.hdr.Control = &oob[0]
		h.hdr.SetControllen(len(oob))
	}
	return true
}

// scopeID — індекс інтерфейсу для link-local IPv6 із зоною.
func scopeID(a netip.Addr) uint32 {
	if z := a.Zone(); z != "" {
		if ifi, err := net.InterfaceByName(z); err == nil {
			return uint32(ifi.Index)
		}
	}
	return 0
}
