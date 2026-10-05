// oo-rollout: O4 staged rollout of agent auto-update (S6 manifests).
//
//	oo-rollout serve -listen 127.0.0.1:8095 -reports /var/lib/oo-rollout/reports.jsonl
//	    приймає POST вердиктів агентів (-auto-update-report-url), токен з env
//	    OO_ROLLOUT_REPORT_TOKEN (обов'язковий, якщо не -insecure-no-token).
//	oo-rollout init -manifest m.json -state st.json   # стартує викатку: stage 0 (канарка)
//	oo-rollout step -manifest m.json -state st.json -reports reports.jsonl
//	    рішення internal/rollout.Step; змінився відсоток -> маніфест
//	    перепідписується (атомарно). Halt -> rollout_percent 0, exit 3.
//
// Ключ підпису — лише env OO_UPDATE_SIGNING_KEY (як у oo-update-sign).
package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/organicoils/oo-screen/internal/autoupdate"
	"github.com/organicoils/oo-screen/internal/rollout"
)

const exitHalt = 3

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	code := 0
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:], time.Now().UTC())
	case "step":
		code, err = cmdStep(os.Args[2:], time.Now().UTC(), os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "oo-rollout:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: oo-rollout serve|init|step [flags]  (див. deploy/DEPLOY.md, розділ O4)")
	os.Exit(2)
}

func signingKey() (ed25519.PrivateKey, error) {
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("OO_UPDATE_SIGNING_KEY")))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("OO_UPDATE_SIGNING_KEY must be a base64 %d-byte seed", ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func planFlags(fs *flag.FlagSet) *rollout.Plan {
	p := rollout.DefaultPlan()
	fs.Func("stages", "percents, e.g. 1,10,50,100 (default 1,10,50,100)", func(s string) error {
		p.Stages = nil
		for _, f := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil {
				return err
			}
			p.Stages = append(p.Stages, n)
		}
		return nil
	})
	fs.DurationVar(&p.MinSoak, "soak", p.MinSoak, "minimum time per stage")
	fs.IntVar(&p.MinReports, "min-reports", p.MinReports, "conclusive reports per stage to advance")
	fs.Float64Var(&p.MaxFailRate, "max-fail-rate", p.MaxFailRate, "fail/(ok+fail) above this halts")
	fs.DurationVar(&p.MaxStageTime, "max-stage-time", p.MaxStageTime, "halt if a stage stays without enough reports this long (0 = never)")
	return &p
}

// readManifest verifies the current manifest with the key we sign with.
func readManifest(path string, priv ed25519.PrivateKey) (*autoupdate.Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return autoupdate.Verify(priv.Public().(ed25519.PublicKey), b)
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".oo-rollout-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func resign(path string, priv ed25519.PrivateKey, m autoupdate.Manifest, pct int, now time.Time) error {
	m.RolloutPercent = pct
	m.IssuedAt = now.Truncate(time.Second)
	env, err := autoupdate.Sign(priv, m)
	if err != nil {
		return err
	}
	return writeAtomic(path, env)
}

func cmdInit(args []string, now time.Time) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	man := fs.String("manifest", "", "signed manifest (re-signed in place)")
	st := fs.String("state", "", "state file to create")
	force := fs.Bool("force", false, "overwrite an existing state (e.g. restart a halted rollout)")
	p := planFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *man == "" || *st == "" {
		return errors.New("-manifest and -state are required")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if _, err := os.Stat(*st); err == nil && !*force {
		return fmt.Errorf("%s exists (use -force)", *st)
	}
	priv, err := signingKey()
	if err != nil {
		return err
	}
	m, err := readManifest(*man, priv)
	if err != nil {
		return err
	}
	s := rollout.State{Version: m.Version, StageStarted: now}
	b, _ := json.MarshalIndent(s, "", "  ")
	if err := resign(*man, priv, *m, p.Stages[0], now); err != nil {
		return err
	}
	return writeAtomic(*st, b)
}

func loadReports(path string) ([]rollout.Report, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []rollout.Report
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r rollout.Report
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

func cmdStep(args []string, now time.Time, out io.Writer) (int, error) {
	fs := flag.NewFlagSet("step", flag.ContinueOnError)
	man := fs.String("manifest", "", "signed manifest (re-signed in place when the percent changes)")
	st := fs.String("state", "", "state file from init")
	reps := fs.String("reports", "", "JSONL written by serve")
	p := planFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 0, err
	}
	if *man == "" || *st == "" || *reps == "" {
		return 0, errors.New("-manifest, -state and -reports are required")
	}
	priv, err := signingKey()
	if err != nil {
		return 0, err
	}
	m, err := readManifest(*man, priv)
	if err != nil {
		return 0, err
	}
	sb, err := os.ReadFile(*st)
	if err != nil {
		return 0, err
	}
	var s rollout.State
	if err := json.Unmarshal(sb, &s); err != nil {
		return 0, err
	}
	if s.Version != m.Version {
		return 0, fmt.Errorf("state is for %s, manifest is %s (run init for the new release)", s.Version, m.Version)
	}
	r, err := loadReports(*reps)
	if err != nil {
		return 0, err
	}
	ns, d, err := rollout.Step(*p, s, r, now)
	if err != nil {
		return 0, err
	}
	if d.Percent != m.RolloutPercent {
		if err := resign(*man, priv, *m, d.Percent, now); err != nil {
			return 0, err
		}
	}
	b, _ := json.MarshalIndent(ns, "", "  ")
	if err := writeAtomic(*st, b); err != nil {
		return 0, err
	}
	fmt.Fprintf(out, "version=%s action=%s percent=%d ok=%d fail=%d inconclusive=%d reason=%q\n",
		ns.Version, d.Action, d.Percent, d.OK, d.Fail, d.Inconcl, d.Reason)
	if d.Action == rollout.Halt {
		return exitHalt, nil
	}
	return 0, nil
}

// reportHandler appends validated reports; the server clock stamps At.
func reportHandler(path, token string, now func() time.Time) http.Handler {
	var mu sync.Mutex
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		var rep rollout.Report
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&rep); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		switch rep.Result {
		case rollout.ResultOK, rollout.ResultFail, rollout.ResultInconclusive:
		default:
			http.Error(w, "bad result", http.StatusBadRequest)
			return
		}
		if rep.Node == "" || len(rep.Node) > 128 || rep.Version == "" || len(rep.Version) > 64 {
			http.Error(w, "bad node/version", http.StatusBadRequest)
			return
		}
		rep.At = now().UTC()
		line, _ := json.Marshal(rep)
		mu.Lock()
		defer mu.Unlock()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			http.Error(w, "storage", http.StatusInternalServerError)
			return
		}
		_, werr := f.Write(append(line, '\n'))
		cerr := f.Close()
		if werr != nil || cerr != nil {
			http.Error(w, "storage", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8095", "listen address (put behind nginx TLS)")
	reps := fs.String("reports", "", "JSONL file to append to")
	insecure := fs.Bool("insecure-no-token", false, "accept reports without OO_ROLLOUT_REPORT_TOKEN (test only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reps == "" {
		return errors.New("-reports is required")
	}
	token := os.Getenv("OO_ROLLOUT_REPORT_TOKEN")
	if token == "" && !*insecure {
		return errors.New("OO_ROLLOUT_REPORT_TOKEN is empty (or pass -insecure-no-token)")
	}
	mux := http.NewServeMux()
	mux.Handle("/rollout/report", reportHandler(*reps, token, time.Now))
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
	log.Printf("oo-rollout: serving /rollout/report on %s -> %s", *listen, *reps)
	return srv.ListenAndServe()
}
