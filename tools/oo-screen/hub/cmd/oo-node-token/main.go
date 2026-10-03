// oo-node-token — випускає агентський токен ноди (SEC #17) без звертання до
// хаба: token = hex(HMAC-SHA256(master, node)). Master читається з файлу
// -secret-file або з env OO_SCREEN_AGENT_SECRET (фолбек OO_SCREEN_T1_TOKEN —
// так само, як у хаба); у командний рядок секрет не передається.
//
//	OO_SCREEN_AGENT_SECRET=... oo-node-token -node PC-042 > token.txt
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/organicoils/oo-screen/hub"
)

func main() {
	node := flag.String("node", "", "node_id, для якого випустити токен (обовʼязково)")
	secretFile := flag.String("secret-file", "", "файл із master-секретом (інакше env OO_SCREEN_AGENT_SECRET / OO_SCREEN_T1_TOKEN)")
	flag.Parse()
	if *node == "" {
		fmt.Fprintln(os.Stderr, "oo-node-token: -node обовʼязковий")
		os.Exit(2)
	}
	master := os.Getenv("OO_SCREEN_AGENT_SECRET")
	if master == "" {
		master = os.Getenv("OO_SCREEN_T1_TOKEN")
	}
	if *secretFile != "" {
		b, err := os.ReadFile(*secretFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "oo-node-token:", err)
			os.Exit(1)
		}
		master = strings.TrimSpace(string(b))
	}
	if master == "" {
		fmt.Fprintln(os.Stderr, "oo-node-token: master-секрет не задано")
		os.Exit(1)
	}
	fmt.Println(hub.NodeToken(master, *node))
}
