//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// logRotateBytes — поріг, після якого поточний лог відкладається як .1.
// A-37: раніше файл відкривався з O_TRUNC — кожен старт стирав історію, тобто
// саме те, що треба читати після падіння, зникало разом із падінням.
const logRotateBytes = 8 << 20

// openAgentLog відкриває лог на дописування з ротацією за розміром; якщо шлях
// недоступний (права на ProgramData, зайнятий файл) — падає у %LOCALAPPDATA%,
// а не мовчить (A-38: невдале відкриття = агент без жодної діагностики).
// Повертає файл і те, куди він реально пише.
func openAgentLog(path string) (*os.File, string, error) {
	if f, err := openRotating(path); err == nil {
		return f, path, nil
	}
	fallback := filepath.Join(os.Getenv("LOCALAPPDATA"), "OrganicOils", "oo-agent.log")
	_ = os.MkdirAll(filepath.Dir(fallback), 0o755)
	f, err := openRotating(fallback)
	if err != nil {
		return nil, "", fmt.Errorf("log %s and fallback %s: %w", path, fallback, err)
	}
	return f, fallback, nil
}

func openRotating(path string) (*os.File, error) {
	if st, err := os.Stat(path); err == nil && st.Size() > logRotateBytes {
		// ponytail: одна генерація .1; глибша історія — коли її хтось читатиме.
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
