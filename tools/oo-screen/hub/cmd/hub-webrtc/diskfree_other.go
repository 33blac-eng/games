//go:build !unix

package main

// diskFreeBytes на не-Unix (у нас це Windows-збірка для локальної розробки й
// тестів) чесно каже «не вмію». Прибирання за віком і обсягом працює скрізь —
// а питання до файлової системи лишається там, де хаб справді живе.
//
// ponytail: стеля — на Windows архів обмежений лише recordMaxBytes. Апгрейд,
// якщо колись хаб поїде під Windows: GetDiskFreeSpaceExW через
// golang.org/x/sys/windows, рівно в цій функції.
func diskFreeBytes(string) (uint64, bool) { return 0, false }
