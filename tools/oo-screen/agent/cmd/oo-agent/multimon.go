// multimon.go — F6: кілька моніторів одночасно (-multimon або
// OO_SCREEN_MULTIMON=1, типово ВИМКНЕНО).
//
// Як це влаштовано. Агент-«батько» лишається рівно тим, чим був: монітор
// -output (зазвичай 0) під node_id ноди, зі звуком, вводом, курсором і
// перемиканням монітора. Для КОЖНОГО ІНШОГО монітора він піднімає
// процес-«дитину» — той самий бінар з -multimon-child=<i> -output=<i>
// -node=<node>#m<i> (internal/multimon). Дитина — повноцінний publisher зі
// своїм капчером, енкодером, WebRTC-ногою, гейтом і реконектом; хаб роздає її
// як окрему ноду (hub/cmd/hub-webrtc/multimon.go).
//
// Чому процеси, а не горутини: кадровий цикл main() тримає десятки глобальних
// станів (outputs, курсор, гейт, бітрейт), а capture.Capturer «NOT safe for
// concurrent use». Процес на монітор ізолює все це без переписування циклу, а
// падіння капчера одного монітора не валить інші.
//
// Дитина: без звуку, без вводу, без шару курсора (їх несе лише основний
// потік — інакше два джерела вводу/звуку на один ПК), select_output ігнорує
// (вона закріплена за своїм монітором), власний мʼютекс одного екземпляра.
// Живе, поки відкритий її stdin — батько тримає трубу; батько помер =>
// EOF => дитина виходить (без job objects і без сиріт).
//
// Один монітор => дітей немає, поведінка ідентична старій.
// ponytail: набір дітей рахується на старті; монітор, підключений пізніше,
// зʼявиться після перезапуску агента (планувальник при логоні).
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/organicoils/oo-screen/internal/multimon"
)

// multimonMaxDefault — типова стеля одночасних потоків (разом з основним).
// Кожен потік = свій апаратний енкодер; споживчі GPU тримають 3–5 сесій NVENC.
const multimonMaxDefault = 3

// multimonEnvOn — вмикання через середовище (прапорець перекриває лише в бік ON).
func multimonEnvOn() bool { return os.Getenv("OO_SCREEN_MULTIMON") == "1" }

// multimonChildren — індекси моніторів для дітей: 1..n-1 крім active, разом
// з основним не більше max і не більше multimon.MaxIndex. Індекс 0 не буває
// дитиною (node_id без суфікса — основний потік); якщо основний стартував з
// -output=k>0, монітор 0 у дітей не потрапляє. n<=1 або max<=1 => nil.
func multimonChildren(n, active, max int) []int {
	if max <= 1 || n <= 1 {
		return nil
	}
	var out []int
	for i := 1; i < n && i <= multimon.MaxIndex && len(out)+1 < max; i++ {
		if i != active {
			out = append(out, i)
		}
	}
	return out
}

// childStrippedFlags — прапорці батька, які дитина НЕ успадковує.
var childStrippedFlags = map[string]struct{}{
	"output": {}, "node": {}, "token": {}, "token-file": {}, "log": {},
	"multimon-child": {}, "multimon-max": {},
	"audio": {}, "input": {}, "cursor-layer": {}, "multimon": {},
}

// flagIsBool — булевість прапорця за реєстром flag.CommandLine.
func flagIsBool(name string) bool {
	f := flag.CommandLine.Lookup(name)
	if f == nil {
		return false
	}
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// multimonChildArgs — аргументи дитини для монітора idx. isBool каже, чи
// прапорець булевий (не їсть наступне слово) — як його бачить пакет flag.
func multimonChildArgs(parent []string, idx int, baseNode, logPath string, isBool func(string) bool) []string {
	var out []string
	for i := 0; i < len(parent); i++ {
		a := parent[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			out = append(out, parent[i:]...)
			break
		}
		name := strings.TrimLeft(a, "-")
		hasVal := false
		if j := strings.IndexByte(name, '='); j >= 0 {
			name, hasVal = name[:j], true
		}
		takesArg := !hasVal && !isBool(name)
		if _, strip := childStrippedFlags[name]; !strip {
			out = append(out, a)
			if takesArg && i+1 < len(parent) {
				i++
				out = append(out, parent[i])
			}
			continue
		}
		if takesArg {
			i++ // значення окремим словом
		}
	}
	out = append(out,
		"-multimon-child="+itoa(idx),
		"-output="+itoa(idx),
		"-node="+multimon.NodeID(baseNode, idx),
	)
	if logPath != "" {
		out = append(out, "-log="+childLogPath(logPath, idx))
	}
	return out
}

// childLogPath — "agent.log" -> "agent.m1.log": окремий файл на дитину.
func childLogPath(p string, idx int) string {
	ext := ""
	if j := strings.LastIndexByte(p, '.'); j > strings.LastIndexAny(p, `/\`) {
		p, ext = p[:j], p[j:]
	}
	return p + ".m" + itoa(idx) + ext
}

// multimonChildEnv — середовище дитини: токен уже розвʼязаний батьком (через
// env, не командний рядок — SEC #33), звук і ввід вимкнені.
func multimonChildEnv(parent []string, token string) []string {
	out := make([]string, 0, len(parent)+3)
	for _, kv := range parent {
		k, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(k) {
		case "OO_AGENT_TOKEN", "OO_SCREEN_AUDIO", "OO_SCREEN_INPUT", "OO_SCREEN_MULTIMON":
			continue
		}
		out = append(out, kv)
	}
	return append(out, "OO_AGENT_TOKEN="+token, "OO_SCREEN_AUDIO=0", "OO_SCREEN_INPUT=0")
}

func itoa(i int) string { return strconv.Itoa(i) }

// superviseChild тримає одну дитину живою до ctx.Done(): start запускає її і
// повертає wait (блокується до виходу). Падіння => перезапуск з бек-офом
// (той самий nextBackoff, що в реконекті); прожила довше за healthy —
// бек-оф скидається.
func superviseChild(ctx context.Context, idx int, start func() (wait func() error, err error)) {
	const healthy = time.Minute
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		began := time.Now()
		wait, err := start()
		if err == nil {
			err = wait()
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > healthy {
			backoff = 0
		}
		if backoff == 0 {
			backoff = time.Second
		} else {
			backoff = nextBackoff(backoff)
		}
		log.Printf("oo-agent: multimon монітор %d: дитина завершилась (%v) — перезапуск через %v", idx, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// watchParentStdin — дитина: EOF/помилка на stdin означає, що батька немає.
func watchParentStdin(r io.Reader, stop func()) {
	_, _ = io.Copy(io.Discard, r)
	log.Printf("oo-agent: multimon: батьківський процес зник — виходжу")
	stop()
}
