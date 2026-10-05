//go:build windows && !wt

package main

import "errors"

// dialWT — легасі-транспорт WT у прод-бінар не зібрано (transport_wt.go): він
// вимикає перевірку TLS. Прапорець -transport wt лишається і чесно каже, як
// зібрати бенч-агента.
func dialWT(string, func(), func(uint64), func(int)) (transport, error) {
	return nil, errors.New("транспорт wt (легасі-бенч) у цей бінар не зібрано: go build -tags wt")
}
