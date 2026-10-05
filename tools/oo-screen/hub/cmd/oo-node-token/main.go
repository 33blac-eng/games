// oo-node-token — адмін-інструмент агентських токенів нод (SEC #17 / S1) без
// звертання до хаба: token = hex(HMAC-SHA256(master, node)). Master читається
// так само, як у хаба: файл -secret-file, env OO_SCREEN_AGENT_SECRET, файл
// OO_SCREEN_AGENT_SECRET_FILE, фолбек OO_SCREEN_T1_TOKEN (з попередженням: такий
// master є на кожному ПК, і strict із ним хаб не запустить). У командний рядок
// секрет не передається.
//
//	oo-node-token -gen-secret > /etc/oo-screen/agent-master   # новий master (chmod 0600)
//	OO_SCREEN_AGENT_SECRET_FILE=/etc/oo-screen/agent-master oo-node-token -node PC-042
//	oo-node-token -secret-file m -nodes-file nodes.txt > tokens.tsv  # пакетно: node<TAB>token
//	oo-node-token -secret-file m -node PC-042 -verify <token>        # код 0 = годиться
//
// Ротація master: новий master → OO_SCREEN_AGENT_SECRET(_FILE), старий →
// OO_SCREEN_AGENT_SECRET_PREV на хабі; перевипустити токени (-nodes-file) і
// роздати агентам; коли в журналі хаба зникнуть WARNING про старий master —
// прибрати _PREV. -verify з -prev-secret-file показує, яким master підписано токен.
package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/organicoils/oo-screen/hub"
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

// maxNodeID — та сама стеля, що й у хаба (validAgentNodeID).
const maxNodeID = 256

// validNode — node_id, який хаб прийме (непорожній, ≤256 байт, без керівних).
func validNode(n string) bool {
	if n == "" || len(n) > maxNodeID {
		return false
	}
	for _, r := range n {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return false
		}
	}
	return true
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("%s: порожній файл", path)
	}
	return v, nil
}

// loadMaster — master у тому ж порядку, що й хаб; fallback=true, якщо це
// спільний T1-токен.
func loadMaster(secretFile string, getenv func(string) string) (master string, fallback bool, err error) {
	if secretFile != "" {
		m, err := readSecret(secretFile)
		return m, false, err
	}
	if v := getenv("OO_SCREEN_AGENT_SECRET"); v != "" {
		return v, false, nil
	}
	if p := getenv("OO_SCREEN_AGENT_SECRET_FILE"); p != "" {
		m, err := readSecret(p)
		return m, false, err
	}
	if v := getenv("OO_SCREEN_T1_TOKEN"); v != "" {
		return v, true, nil
	}
	return "", false, errors.New("master-секрет не задано (-secret-file, OO_SCREEN_AGENT_SECRET, OO_SCREEN_AGENT_SECRET_FILE)")
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oo-node-token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	node := fs.String("node", "", "node_id, для якого випустити/перевірити токен")
	nodesFile := fs.String("nodes-file", "", "файл зі списком node_id (по одному на рядок, # — коментар); вивід: node<TAB>token")
	secretFile := fs.String("secret-file", "", "файл із master-секретом (інакше env OO_SCREEN_AGENT_SECRET / OO_SCREEN_AGENT_SECRET_FILE / OO_SCREEN_T1_TOKEN)")
	prevSecretFile := fs.String("prev-secret-file", "", "для -verify: файл зі СТАРИМ master (ротація)")
	verify := fs.String("verify", "", "перевірити цей токен для -node (код виходу 0 = годиться)")
	genSecret := fs.Bool("gen-secret", false, "надрукувати новий випадковий master (32 байти hex) і вийти")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *genSecret {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintln(stderr, "oo-node-token:", err)
			return 1
		}
		fmt.Fprintln(stdout, hex.EncodeToString(b))
		return 0
	}
	if (*node == "") == (*nodesFile == "") {
		fmt.Fprintln(stderr, "oo-node-token: потрібен рівно один із -node або -nodes-file")
		return 2
	}
	if *verify != "" && *node == "" {
		fmt.Fprintln(stderr, "oo-node-token: -verify працює лише з -node")
		return 2
	}
	if *node != "" && !validNode(*node) {
		fmt.Fprintf(stderr, "oo-node-token: недопустимий node_id %q (порожній, >%d байт або керівні символи — хаб такий відкине)\n", *node, maxNodeID)
		return 2
	}
	master, fallback, err := loadMaster(*secretFile, getenv)
	if err != nil {
		fmt.Fprintln(stderr, "oo-node-token:", err)
		return 1
	}
	if fallback {
		fmt.Fprintln(stderr, "oo-node-token: WARNING master = OO_SCREEN_T1_TOKEN (спільний для всіх ПК); з таким master хаб у strict не стартує — згенеруй окремий: -gen-secret")
	}
	if *verify != "" {
		got := strings.TrimSpace(*verify)
		if hub.NodeTokenValid(master, *node, got) {
			fmt.Fprintln(stdout, "ok: поточний master")
			return 0
		}
		if *prevSecretFile != "" {
			prev, err := readSecret(*prevSecretFile)
			if err != nil {
				fmt.Fprintln(stderr, "oo-node-token:", err)
				return 1
			}
			if hub.NodeTokenValid(prev, *node, got) {
				fmt.Fprintln(stdout, "ok: СТАРИЙ master — перевипусти токен до зняття OO_SCREEN_AGENT_SECRET_PREV")
				return 0
			}
		}
		fmt.Fprintf(stdout, "FAIL: токен не належить ноді %q (інший node_id, master чи пошкоджений)\n", *node)
		return 1
	}
	if *node != "" {
		fmt.Fprintln(stdout, hub.NodeToken(master, *node))
		return 0
	}
	f, err := os.Open(*nodesFile)
	if err != nil {
		fmt.Fprintln(stderr, "oo-node-token:", err)
		return 1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	seen := map[string]bool{}
	bad, line := 0, 0
	for sc.Scan() {
		line++
		n := strings.TrimSpace(sc.Text())
		if n == "" || strings.HasPrefix(n, "#") {
			continue
		}
		if !validNode(n) {
			fmt.Fprintf(stderr, "oo-node-token: %s:%d: недопустимий node_id %q — пропущено\n", *nodesFile, line, n)
			bad++
			continue
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		fmt.Fprintf(stdout, "%s\t%s\n", n, hub.NodeToken(master, n))
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(stderr, "oo-node-token:", err)
		return 1
	}
	if bad > 0 {
		return 1
	}
	return 0
}
