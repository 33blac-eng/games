#!/usr/bin/env python3
"""bench/run_matrix.py — матричний ранер трас S2..S7 x кандидати A/B через
VPS-hub з netem-імпейрментом (stdlib only, без нових залежностей).

Для кожного (trace, candidate, run):
  1. на VPS: netem.sh apply <trace>  (для S7: s7-start, потім через 60с s7-collapse)
  2. локально: піднімає player відповідного кандидата, спрямований на VPS-hub
     - A: agent/cmd/corpus-player-webrtc -> OO_SCREEN_HUB_URL=http://<vps>:4470/offer/agent
     - B: agent/cmd/corpus-player-wt -hub <vps>:4460
  3. bench/capture.py --candidate {A,B} --url <viewer-url з ?hub=/?url= на VPS> --seconds N
     (viewer грузиться з ЛОКАЛЬНОГО web/ static-сервера — playwright headed
     все одно open-ить сторінку локально, лише offer/RTP йде на VPS)
  4. гасить player
  5. на VPS: netem.sh clear

Usage:
  python bench/run_matrix.py --traces S2,S3,S4,S5,S6,S7 --candidates A,B \
      --runs 3 --seconds 60 --hub-host 185.166.216.204

Токени: OO_SCREEN_T1_TOKEN (той самий, що в обох VPS-юнітах).
Шлях до OO_ERP-репо (для _vps) захардкоджений нижче — там єдине джерело
vps_exec/vps_sftp у цьому дереві.
"""
import argparse
import http.server
import os
import subprocess
import sys
import threading
import time
import pathlib

ROOT = pathlib.Path(__file__).resolve().parent.parent
OO_ERP_DIR = "D:/Claude/ORGANICOILS WEBSITE/OO_ERP"
DEFAULT_TOKEN = os.environ.get("OO_SCREEN_T1_TOKEN", "t1-dev-token")
WEB_PORT = 4480

ALL_TRACES = ["S2", "S3", "S4", "S5", "S6", "S7"]


def vps_exec(cmd: str) -> str:
    """Runs a shell command on the VPS via deploy/_vps.py, returns stdout.
    Raises on non-zero exit."""
    py = (
        "import sys;sys.path.insert(0,'deploy');import _vps;"
        f"rc,out,err=_vps.vps_exec({cmd!r});"
        "sys.stdout.write(out);"
        "sys.stderr.write(err);"
        "sys.exit(0 if rc==0 else 1)"
    )
    r = subprocess.run(
        [sys.executable, "-c", py], cwd=OO_ERP_DIR,
        capture_output=True, text=True,
    )
    if r.returncode != 0:
        raise RuntimeError(f"vps_exec failed: {cmd!r}\nstdout={r.stdout}\nstderr={r.stderr}")
    return r.stdout


def netem_apply(trace: str):
    if trace == "S7":
        print(f"[netem] s7-start", flush=True)
        vps_exec("/opt/oo-screen/netem.sh s7-start")
    else:
        print(f"[netem] apply {trace}", flush=True)
        vps_exec(f"/opt/oo-screen/netem.sh apply {trace}")


def netem_s7_collapse():
    print("[netem] s7-collapse", flush=True)
    vps_exec("/opt/oo-screen/netem.sh s7-collapse")


def netem_clear():
    print("[netem] clear", flush=True)
    vps_exec("/opt/oo-screen/netem.sh clear")


def netem_status() -> str:
    return vps_exec("/opt/oo-screen/netem.sh status")


def get_certhash() -> str:
    """Pulls the latest CERT_HASH= line for hub-wt (candidate B) from the VPS
    unit's journal — never hardcode it, the cert regenerates on hub-wt restart."""
    out = vps_exec("journalctl -u oo-screen-hub-t1 --no-pager | grep CERT_HASH | tail -1")
    line = out.strip().splitlines()[-1] if out.strip() else ""
    if "CERT_HASH=" not in line:
        raise RuntimeError(f"could not find CERT_HASH= in oo-screen-hub-t1 journal: {out!r}")
    return line.split("CERT_HASH=", 1)[1].strip()


def _make_quiet_handler(directory: str):
    import functools

    class _QuietHandler(http.server.SimpleHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

    return functools.partial(_QuietHandler, directory=directory)


def start_web_server():
    """Serves web/ on 127.0.0.1:4480 in a background thread, if not already
    listening (matches the convention in run_a.sh/run_s1.sh).

    IMPORTANT: pass directory= explicitly instead of os.chdir()-ing around the
    server start — SimpleHTTPRequestHandler resolves paths from the process's
    CURRENT cwd at REQUEST time (not at server-construction time), so a
    chdir-then-chdir-back dance here would silently 404 every request once the
    process cwd moves back to ROOT (found by hand while debugging why every
    matrix run's viewer page loaded blank)."""
    import socket
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    try:
        s.bind(("127.0.0.1", WEB_PORT))
        s.close()
    except OSError:
        print(f"[web] :{WEB_PORT} already in use, assuming static server is up", flush=True)
        return None

    handler = _make_quiet_handler(str(ROOT / "web"))
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", WEB_PORT), handler)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    print(f"[web] serving web/ on :{WEB_PORT}", flush=True)
    return httpd


def build_binaries():
    tmp = ROOT / "build" / "matrix-run"
    tmp.mkdir(parents=True, exist_ok=True)
    for name, pkg in [
        ("player-webrtc.exe", "agent/cmd/corpus-player-webrtc"),
        ("player-wt.exe", "agent/cmd/corpus-player-wt"),
    ]:
        out = tmp / name
        print(f"[build] {pkg} -> {out}", flush=True)
        r = subprocess.run(["go", "build", "-o", str(out), f"./{pkg}"], cwd=ROOT)
        if r.returncode != 0:
            raise RuntimeError(f"go build failed for {pkg}")
    return tmp


def start_player_a(bin_dir: pathlib.Path, hub_host: str, token: str, log_path: pathlib.Path):
    env = dict(os.environ)
    env["OO_SCREEN_HUB_URL"] = f"http://{hub_host}:4470/offer/agent"
    env["OO_SCREEN_T1_TOKEN"] = token
    log = open(log_path, "w")
    return subprocess.Popen([str(bin_dir / "player-webrtc.exe")], cwd=ROOT, env=env,
                             stdout=log, stderr=subprocess.STDOUT)


def start_player_b(bin_dir: pathlib.Path, hub_host: str, token: str, log_path: pathlib.Path):
    env = dict(os.environ)
    env["OO_SCREEN_T1_TOKEN"] = token
    log = open(log_path, "w")
    return subprocess.Popen(
        [str(bin_dir / "player-wt.exe"), "-hub", f"{hub_host}:4460",
         "-corpus", "bench/corpus/corpus-1080p60.h264"],
        cwd=ROOT, env=env, stdout=log, stderr=subprocess.STDOUT,
    )


def viewer_url_a(hub_host: str, token: str) -> str:
    return f"http://127.0.0.1:{WEB_PORT}/viewer-webrtc.html?token={token}&hub={hub_host}:4470"


def viewer_url_b(hub_host: str, token: str, certhash: str) -> str:
    import urllib.parse
    certhash_enc = urllib.parse.quote(certhash, safe="")
    wt_url = urllib.parse.quote(f"https://{hub_host}:4461/wt", safe="")
    return (f"http://127.0.0.1:{WEB_PORT}/viewer-wt.html?"
            f"url={wt_url}&token={token}&certhash={certhash_enc}")


def run_capture(candidate: str, url: str, seconds: int, out_path: pathlib.Path) -> bool:
    cmd = [sys.executable, "bench/capture.py", "--candidate", candidate,
           "--url", url, "--seconds", str(seconds), "--out", str(out_path)]
    r = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True)
    ok = r.returncode == 0 and "CAPTURE_OK" in r.stdout
    print(r.stdout.strip())
    if r.stderr.strip():
        print(r.stderr.strip(), file=sys.stderr)
    return ok


def run_one(trace: str, cand: str, run_no: int, bin_dir: pathlib.Path, hub_host: str,
            token: str, seconds: int, certhash: str | None):
    tag = f"TRACE={trace} CAND={cand} RUN={run_no}"
    print(f"[{tag}] start", flush=True)
    out_dir = ROOT / "results" / trace / cand
    out_dir.mkdir(parents=True, exist_ok=True)
    out_path = out_dir / f"run{run_no}.ndjson"
    log_dir = ROOT / "results" / "_logs"
    log_dir.mkdir(parents=True, exist_ok=True)
    player_log = log_dir / f"{trace}-{cand}-run{run_no}-player.log"

    try:
        netem_apply(trace)

        collapse_timer = None
        if trace == "S7":
            collapse_timer = threading.Timer(60.0, netem_s7_collapse)
            collapse_timer.start()

        if cand == "A":
            proc = start_player_a(bin_dir, hub_host, token, player_log)
            url = viewer_url_a(hub_host, token)
        else:
            proc = start_player_b(bin_dir, hub_host, token, player_log)
            url = viewer_url_b(hub_host, token, certhash)

        time.sleep(2)  # даємо player-у зʼєднатись перед capture.py

        ok = run_capture(cand, url, seconds, out_path)

        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

        if collapse_timer is not None:
            collapse_timer.cancel()  # no-op if already fired

        status = "done" if ok else "fail"
        print(f"[{tag}] {status} out={out_path}", flush=True)
        return ok
    except Exception as e:
        print(f"[{tag}] fail exception={e}", flush=True)
        return False
    finally:
        netem_clear()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--traces", default=",".join(ALL_TRACES))
    ap.add_argument("--candidates", default="A,B")
    ap.add_argument("--runs", type=int, default=1)
    ap.add_argument("--seconds", type=int, default=60)
    ap.add_argument("--hub-host", default="185.166.216.204")
    ap.add_argument("--token", default=DEFAULT_TOKEN)
    args = ap.parse_args()

    traces = [t.strip() for t in args.traces.split(",") if t.strip()]
    candidates = [c.strip() for c in args.candidates.split(",") if c.strip()]

    bin_dir = build_binaries()
    start_web_server()

    certhash = None
    if "B" in candidates:
        certhash = get_certhash()
        print(f"[setup] certhash={certhash}", flush=True)

    results = []
    for trace in traces:
        for cand in candidates:
            for run_no in range(1, args.runs + 1):
                ok = run_one(trace, cand, run_no, bin_dir, args.hub_host, args.token,
                             args.seconds, certhash)
                results.append((trace, cand, run_no, ok))

    print("\n=== MATRIX SUMMARY ===")
    n_fail = 0
    for trace, cand, run_no, ok in results:
        status = "OK" if ok else "FAIL"
        if not ok:
            n_fail += 1
        print(f"  {trace} {cand} run{run_no}: {status}")

    print(f"\n{len(results) - n_fail}/{len(results)} runs OK")
    print("\nAnalyze with, e.g.:")
    print("  python bench/score.py --traces " + ",".join(traces) + " --candidates " + ",".join(candidates))
    print("  python bench/analyze.py --file results/<TRACE>/<CAND>/run1.ndjson")

    print("\n[final] confirming netem is cleared on VPS:")
    print(netem_status())

    return 1 if n_fail else 0


if __name__ == "__main__":
    sys.exit(main())
