// oo-rec-decrypt розшифровує запис сесії (*.mkv.enc, S5) у звичайний MKV.
//
//	oo-rec-decrypt -key-file /etc/oo-screen/record.key in.mkv.enc > out.mkv
//	OO_SCREEN_RECORD_KEY=<hex> oo-rec-decrypt in.mkv.enc -o out.mkv
//
// Обрізаний хвіст (хаб убили посеред запису): усе до нього пишеться, код
// виходу 2 і попередження в stderr — MKV і так читабельний до останнього
// цілого кластера.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/organicoils/oo-screen/internal/reccrypt"
)

func main() {
	keyFile := flag.String("key-file", os.Getenv("OO_SCREEN_RECORD_KEY_FILE"), "файл ключа (32 байти сирі, hex або base64; права 0600)")
	outPath := flag.String("o", "", "куди писати (типово stdout)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "використання: oo-rec-decrypt [-key-file K] [-o out.mkv] in.mkv.enc")
		os.Exit(64)
	}
	key, err := reccrypt.LoadKey(os.Getenv("OO_SCREEN_RECORD_KEY"), *keyFile)
	if err != nil || key == nil {
		fmt.Fprintln(os.Stderr, "ключ:", err, "(задайте -key-file або OO_SCREEN_RECORD_KEY)")
		os.Exit(64)
	}
	in, err := os.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer in.Close()
	var out io.Writer = os.Stdout
	if *outPath != "" {
		f, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}
	n, err := reccrypt.Decrypt(out, in, key)
	switch {
	case errors.Is(err, reccrypt.ErrTruncated):
		fmt.Fprintf(os.Stderr, "попередження: файл обрізано, розшифровано %d байтів до обриву\n", n)
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
