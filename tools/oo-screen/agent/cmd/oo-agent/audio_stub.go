//go:build !windows

package main

import "os"

// audioEnabled — той самий прапорець, що в хабі (audio.go для Windows).
// Стаб лише тримає non-Windows збірку зеленою (agent все одно потребує
// Windows, main_stub.go); саму змінну не скасовує, читає той самий env.
var audioEnabled = os.Getenv("OO_SCREEN_AUDIO") == "1"
