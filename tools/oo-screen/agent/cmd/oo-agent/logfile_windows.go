//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// logRotateBytes — поріг, після якого поточний лог відкладається як .1.
// A-37: раніше файл відкривався з O_TRUNC — кожен старт стирав історію, тобто
// саме те, що треба читати після падіння, зникало разом із падінням.
const logRotateBytes = 8 << 20

// logRotateRetry — як часто пробувати ротацію знову, коли вона не вдалась
// (файл тримає інший процес без FILE_SHARE_DELETE — антивірус, блокнот).
// Без паузи кожен рядок логу коштував би close+rename+reopen.
const logRotateRetry = time.Minute

// openAgentLog відкриває лог на дописування з ротацією за розміром; якщо шлях
// недоступний (права на ProgramData, зайнятий файл) — падає у %LOCALAPPDATA%,
// а не мовчить (A-38: невдале відкриття = агент без жодної діагностики).
// Повертає лог і те, куди він реально пише.
func openAgentLog(path string) (*rotatingLog, string, error) {
	if l, err := openRotating(path, logRotateBytes); err == nil {
		return l, path, nil
	}
	fallback := filepath.Join(os.Getenv("LOCALAPPDATA"), "OrganicOils", "oo-agent.log")
	_ = os.MkdirAll(filepath.Dir(fallback), 0o755)
	l, err := openRotating(fallback, logRotateBytes)
	if err != nil {
		return nil, "", fmt.Errorf("log %s and fallback %s: %w", path, fallback, err)
	}
	return l, fallback, nil
}

// rotatingLog — файл логу, що ротується НА ХОДУ, а не лише на старті: агент
// живе добами, і ротація при запуску лишала файл рости без стелі до наступного
// рестарту.
// ponytail: одна генерація .1; глибша історія — коли її хтось читатиме.
type rotatingLog struct {
	mu      sync.Mutex
	path    string
	limit   int64
	f       *os.File // nil = не відкрився; наступний Write пробує знову
	size    int64
	retryAt time.Time // ротація не вдалась — до цього часу пишемо в той самий файл
}

func openRotating(path string, limit int64) (*rotatingLog, error) {
	l := &rotatingLog{path: path, limit: limit}
	if st, err := os.Stat(path); err == nil && st.Size() > limit {
		_ = l.rotate()
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// rotate відкладає поточний файл як .1. Попередній .1 заміщується самим
// Rename (MoveFileEx з REPLACE_EXISTING) і лише тоді, коли той справді
// відбувся: окремий Remove наперед стирав відкладену історію навіть тоді,
// коли Rename потім падав.
func (l *rotatingLog) rotate() error {
	return os.Rename(l.path, l.path+".1")
}

// open відкриває поточний файл на дописування.
func (l *rotatingLog) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
	return nil
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil && l.size+int64(len(p)) > l.limit && l.size > 0 && !time.Now().Before(l.retryAt) {
		// Rename працює лише на закритому файлі (Windows).
		_ = l.f.Close()
		l.f = nil
		if l.rotate() != nil {
			l.retryAt = time.Now().Add(logRotateRetry)
		}
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	return l.f.Close()
}
