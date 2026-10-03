// Package agentcred — облікові дані агента oo-agent, винесені з Windows-only
// main, щоб їх тестувати на будь-якій платформі.
//
// SEC #33: токен більше не мусить їхати в командному рядку schtask (його бачить
// будь-який локальний користувач у диспетчері задач / WMI). Порядок джерел:
//
//  1. -token-file <шлях>  — файл з ACL SYSTEM/Administrators (рекомендовано);
//  2. env OO_AGENT_TOKEN;
//  3. -token <рядок>      — сумісність, з попередженням у журнал;
//  4. env OO_SCREEN_T1_TOKEN (легасі);
//  5. "t1-dev-token" (стенд).
package agentcred

import (
	"fmt"
	"os"
	"strings"
)

// DevToken — дефолт стенда, коли не задано нічого.
const DevToken = "t1-dev-token"

// Source — звідки взято токен (для журналу; сам токен не логуємо).
type Source string

const (
	SourceFile    Source = "token-file"
	SourceEnv     Source = "env OO_AGENT_TOKEN"
	SourceFlag    Source = "-token (command line)"
	SourceLegacyE Source = "env OO_SCREEN_T1_TOKEN"
	SourceDev     Source = "dev default"
)

// ResolveToken обирає токен за порядком вище. getenv — os.Getenv у бою.
// Помилка — лише якщо -token-file задано, але прочитати/порожній: тихо впасти
// на слабше джерело тут гірше, ніж не стартувати.
func ResolveToken(flagToken, tokenFile string, getenv func(string) string) (string, Source, error) {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", "", fmt.Errorf("token-file: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", "", fmt.Errorf("token-file %s порожній", tokenFile)
		}
		return t, SourceFile, nil
	}
	if v := strings.TrimSpace(getenv("OO_AGENT_TOKEN")); v != "" {
		return v, SourceEnv, nil
	}
	if flagToken != "" {
		return flagToken, SourceFlag, nil
	}
	if v := getenv("OO_SCREEN_T1_TOKEN"); v != "" {
		return v, SourceLegacyE, nil
	}
	return DevToken, SourceDev, nil
}
