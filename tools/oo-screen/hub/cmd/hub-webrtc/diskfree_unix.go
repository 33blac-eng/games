//go:build unix

package main

import "syscall"

// diskFreeBytes — скільки вільно на розділі, де лежить dir. Бойовий хаб — це
// Linux, тож саме тут запобіжник місця й працює по-справжньому.
//
// Bavail, а не Bfree: перше — доступне звичайному процесу, друге включає
// резерв суперкористувача (типово 5% ext4). Різниця в кілька гігабайтів рівно
// в той момент, коли вона найдорожча.
func diskFreeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
