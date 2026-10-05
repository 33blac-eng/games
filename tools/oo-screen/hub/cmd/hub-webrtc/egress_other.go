//go:build !linux

package main

import (
	"errors"
	"net"
	"net/netip"
)

// Поза Linux GSO немає: egressConn лишається єдиним писарем без бійки за
// fdMutex, але шле по пакету.
func gsoSupported(*net.UDPConn) bool { return false }

func writeGSO(*net.UDPConn, []byte, int, netip.AddrPort) error {
	return errors.New("UDP GSO недоступний на цій ОС")
}

type mmsgState struct{}

func (*mmsgState) init(*net.UDPConn) bool { return false }

func (*mmsgState) send([]egressMsg) (int, error) { return 0, errNoMmsg }
