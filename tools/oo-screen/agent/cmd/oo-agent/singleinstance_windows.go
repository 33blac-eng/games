//go:build windows

package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// acquireSingleInstance тримає іменований мʼютекс на час життя процесу.
// A-36: без нього після швидкого релогону (onlogon-задача + ще живий старий
// процес) або ручного старту на ПК крутились ДВА агенти на один DXGI-вивід —
// другий рвав дублікацію першого, і картинка «моргала» без жодної помилки.
// Повертає release і чи вже є інший екземпляр.
func acquireSingleInstance() (release func(), duplicate bool) {
	return acquireNamedInstance(`Global\oo-screen-agent`)
}

// acquireNamedInstance — те саме під довільним іменем: тест мусить перевіряти
// САМ механізм, не займаючи бойове імʼя (інакше живий агент на цій же машині
// зробив би тест то зеленим, то червоним).
func acquireNamedInstance(mutexName string) (release func(), duplicate bool) {
	name := windows.StringToUTF16Ptr(mutexName)
	h, err := windows.CreateMutex(nil, false, name)
	if h != 0 && errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(h)
		return func() {}, true
	}
	if h == 0 {
		// Мʼютекс не створився (екзотика: політика обʼєктів) — не блокуємо старт,
		// але й не прикидаємось, що захист є.
		return func() {}, false
	}
	return func() { windows.CloseHandle(h) }, false
}
