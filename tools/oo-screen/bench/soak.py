#!/usr/bin/env python3
"""bench/soak.py — 8-годинний soak-харнес для кандидата A (WebRTC/Pion) oo-screen.

Гейт Додатка A перед "production": приріст RSS hub-webrtc <= 200MB за 8 год,
нуль вікон-дедлоків стріму (viewer перестав отримувати кадри), стабільні
handle-count (goroutine-лік — лише якщо hub колись почне експонувати
/debug/pprof; наразі hub-webrtc pprof НЕ реєструє, тож goroutines=null).

Піднімає ЛОКАЛЬНИЙ стек (hub-webrtc T1 static-token режим, БЕЗ
OO_SCREEN_ERP_BASE) -> НЕ бойовий VPS-hub. hub-webrtc слухає жорстко
захардкоджений ":4470" (const listenAddr у hub/cmd/hub-webrtc/main.go) —
soak.py це не міняє (поза скоупом чіпати hub-код), просто запускає локальний
процес на цьому порту і завершує його по собі в кінці прогону.

Stdlib-only; psutil використовується ЛИШЕ якщо вже імпортується (без pip
install) — інакше fallback на PowerShell Get-Process для RSS/handle-count на
Windows.

Використання:
  python bench/soak.py --hours 8 --sample-min 5
  python bench/soak.py --quick        # 10-хвилинний самотест харнеса
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

try:
    import psutil  # type: ignore
    HAVE_PSUTIL = True
except ImportError:
    HAVE_PSUTIL = False

ROOT = Path(__file__).resolve().parent.parent  # tools/oo-screen
BENCH = ROOT / "bench"
BUILD_DIR = ROOT / "build" / "soak"
HUB_URL = "http://127.0.0.1:4470/offer/agent"
VIEWER_URL = "http://127.0.0.1:4470/offer/viewer"
TOKEN = os.environ.get("OO_SCREEN_T1_TOKEN", "t1-dev-token")
RSS_GATE_MB = 200.0


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def log(msg: str) -> None:
    print(f"[soak {now_iso()}] {msg}", flush=True)


def run(cmd, **kw):
    log("$ " + " ".join(str(c) for c in cmd))
    return subprocess.run(cmd, cwd=ROOT, check=True, **kw)


def build_binaries() -> dict:
    BUILD_DIR.mkdir(parents=True, exist_ok=True)
    exe = ".exe" if os.name == "nt" else ""
    hub_bin = BUILD_DIR / f"hub-webrtc{exe}"
    agent_bin = BUILD_DIR / f"corpus-player-webrtc{exe}"
    viewer_bin = BUILD_DIR / f"soak_probe{exe}"
    log("building hub-webrtc, corpus-player-webrtc, soak_probe ...")
    run(["go", "build", "-o", str(hub_bin), "./hub/cmd/hub-webrtc"])
    run(["go", "build", "-o", str(agent_bin), "./agent/cmd/corpus-player-webrtc"])
    run(["go", "build", "-o", str(viewer_bin), "./bench/soak_probe.go"])
    return {"hub": hub_bin, "agent": agent_bin, "viewer": viewer_bin}


def rss_handles_windows(pid: int):
    """Fallback RSS(MB)/handle-count via PowerShell Get-Process (no psutil)."""
    ps = (
        f"$p = Get-Process -Id {pid} -ErrorAction SilentlyContinue; "
        "if ($p) { \"$($p.WorkingSet64) $($p.HandleCount)\" } else { \"0 0\" }"
    )
    try:
        out = subprocess.check_output(
            ["powershell", "-NoProfile", "-Command", ps], text=True, timeout=15
        ).strip()
        ws, handles = out.split()
        return float(ws) / (1024 * 1024), int(handles)
    except Exception as e:
        log(f"WARN: powershell RSS/handle sample failed: {e}")
        return None, None


def sample_process(pid: int):
    if HAVE_PSUTIL:
        try:
            p = psutil.Process(pid)
            rss_mb = p.memory_info().rss / (1024 * 1024)
            handles = p.num_handles() if hasattr(p, "num_handles") else None
            return rss_mb, handles
        except Exception as e:
            log(f"WARN: psutil sample failed: {e}")
            return None, None
    if os.name == "nt":
        return rss_handles_windows(pid)
    # POSIX fallback without psutil: /proc
    try:
        with open(f"/proc/{pid}/status") as f:
            text = f.read()
        vmrss = [l for l in text.splitlines() if l.startswith("VmRSS:")]
        rss_mb = int(vmrss[0].split()[1]) / 1024 if vmrss else None
        fd_count = len(os.listdir(f"/proc/{pid}/fd"))
        return rss_mb, fd_count
    except Exception as e:
        log(f"WARN: /proc sample failed: {e}")
        return None, None


def try_goroutines(pprof_base="http://127.0.0.1:4470"):
    """hub-webrtc does NOT register net/http/pprof (checked: no import of
    net/http/pprof anywhere under hub/cmd/hub-webrtc). This is a defensive
    probe in case that changes later; returns None today."""
    import urllib.request

    url = pprof_base + "/debug/pprof/goroutine?debug=1"
    try:
        with urllib.request.urlopen(url, timeout=2) as resp:
            text = resp.read().decode("utf-8", "replace")
        for line in text.splitlines():
            if line.startswith("goroutine profile: total"):
                return int(line.split()[-1])
    except Exception:
        return None
    return None


def read_viewer_state(state_path: Path):
    try:
        data = json.loads(state_path.read_text())
        return int(data.get("count", 0)), bool(data.get("connected", False))
    except Exception:
        return None, None


def wait_tcp(host, port, timeout=15.0):
    import socket

    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with socket.create_connection((host, port), timeout=1):
                return True
        except OSError:
            time.sleep(0.3)
    return False


def terminate(proc: subprocess.Popen, name: str, grace=5.0):
    if proc.poll() is not None:
        return
    log(f"stopping {name} (pid {proc.pid}) ...")
    try:
        proc.terminate()
        proc.wait(timeout=grace)
    except Exception:
        try:
            proc.kill()
        except Exception:
            pass


def main():
    ap = argparse.ArgumentParser(description="oo-screen candidate-A 8h soak harness")
    ap.add_argument("--hours", type=float, default=8.0)
    ap.add_argument("--sample-min", type=float, default=5.0)
    ap.add_argument("--quick", action="store_true", help="10-minute harness self-test, overrides --hours/--sample-min")
    args = ap.parse_args()

    if args.quick:
        total_minutes = 10.0
        sample_min = 1.0
    else:
        total_minutes = args.hours * 60.0
        sample_min = args.sample_min

    ts = datetime.now().strftime("%Y%m%d-%H%M%S")
    ndjson_path = BENCH / f"soak-{ts}.ndjson"
    state_path = BENCH / "soak_viewer_state.json"
    if state_path.exists():
        state_path.unlink()

    bins = build_binaries()

    procs = {}
    log_files = []
    try:
        hub_log = open(BUILD_DIR / "hub.log", "w")
        log_files.append(hub_log)
        log("starting local hub-webrtc (T1 static-token, no OO_SCREEN_ERP_BASE) on :4470 ...")
        env = dict(os.environ)
        env.pop("OO_SCREEN_ERP_BASE", None)
        env["OO_SCREEN_T1_TOKEN"] = TOKEN
        procs["hub"] = subprocess.Popen(
            [str(bins["hub"])], cwd=ROOT, stdout=hub_log, stderr=subprocess.STDOUT, env=env
        )

        if not wait_tcp("127.0.0.1", 4470, timeout=15):
            raise RuntimeError("hub-webrtc did not open :4470 within 15s — see build/soak/hub.log")
        log("hub-webrtc is up.")

        agent_log = open(BUILD_DIR / "agent.log", "w")
        log_files.append(agent_log)
        log("starting corpus-player-webrtc (publisher) ...")
        env2 = dict(os.environ)
        env2["OO_SCREEN_T1_TOKEN"] = TOKEN
        env2["OO_SCREEN_HUB_URL"] = HUB_URL
        procs["agent"] = subprocess.Popen(
            [str(bins["agent"])], cwd=ROOT, stdout=agent_log, stderr=subprocess.STDOUT, env=env2
        )
        time.sleep(2)

        viewer_log = open(BUILD_DIR / "viewer.log", "w")
        log_files.append(viewer_log)
        log("starting soak_probe (viewer, holds connection, counts RTP packets) ...")
        procs["viewer"] = subprocess.Popen(
            [
                str(bins["viewer"]),
                "-signal", VIEWER_URL,
                "-token", TOKEN,
                "-state", str(state_path),
                "-write-every", "5s",
            ],
            cwd=ROOT, stdout=viewer_log, stderr=subprocess.STDOUT,
        )

        # даємо стеку піднятись і viewer-у підключитись
        deadline = time.time() + 20
        while time.time() < deadline and not state_path.exists():
            time.sleep(0.5)
        if not state_path.exists():
            raise RuntimeError("soak_probe never wrote a state file — viewer failed to connect; see build/soak/viewer.log")
        log("viewer connected, state file present. Beginning sampling loop.")

        hub_pid = procs["hub"].pid
        samples = []
        baseline_rss = None
        deadlock_windows = 0
        last_count = None

        n_samples = max(1, int(round(total_minutes / sample_min)))
        for i in range(n_samples):
            time.sleep(sample_min * 60)

            for name, p in procs.items():
                if p.poll() is not None:
                    raise RuntimeError(f"{name} process exited early (code {p.returncode}) — see build/soak/{name}.log")

            rss_mb, handles = sample_process(hub_pid)
            goroutines = try_goroutines()
            count, connected = read_viewer_state(state_path)

            frames_delta = None
            if count is not None:
                frames_delta = count if last_count is None else count - last_count
                last_count = count

            if baseline_rss is None and rss_mb is not None:
                baseline_rss = rss_mb

            alive = bool(connected) and (frames_delta is None or frames_delta > 0)
            if frames_delta == 0:
                deadlock_windows += 1

            rec = {
                "t": now_iso(),
                "rss_mb": round(rss_mb, 2) if rss_mb is not None else None,
                "handles": handles,
                "goroutines": goroutines,
                "frames_delta": frames_delta,
                "alive": alive,
            }
            samples.append(rec)
            with open(ndjson_path, "a", encoding="utf-8") as f:
                f.write(json.dumps(rec) + "\n")
            log(f"sample {i+1}/{n_samples}: rss={rec['rss_mb']}MB handles={rec['handles']} "
                f"goroutines={rec['goroutines']} frames_delta={rec['frames_delta']} alive={rec['alive']}")

        # --- вердикт ---
        rss_values = [s["rss_mb"] for s in samples if s["rss_mb"] is not None]
        growth = (rss_values[-1] - rss_values[0]) if len(rss_values) >= 2 else 0.0
        rss_pass = growth <= RSS_GATE_MB
        deadlock_pass = deadlock_windows == 0

        print("")
        print("=" * 70)
        print(f"SOAK VERDICT ({ndjson_path.name}, {len(samples)} samples, "
              f"{total_minutes:.0f} min @ {sample_min:.1f} min/sample)")
        print(f"  RSS baseline: {rss_values[0] if rss_values else 'n/a'} MB")
        print(f"  RSS final:    {rss_values[-1] if rss_values else 'n/a'} MB")
        print(f"  RSS growth:   {growth:.2f} MB (gate <= {RSS_GATE_MB} MB) -> "
              f"{'PASS' if rss_pass else 'FAIL'}")
        print(f"  Deadlock windows (frames_delta==0): {deadlock_windows} -> "
              f"{'PASS' if deadlock_pass else 'FAIL'}")
        print(f"  Overall: {'PASS' if (rss_pass and deadlock_pass) else 'FAIL'}")
        print("=" * 70)

        return 0 if (rss_pass and deadlock_pass) else 1

    finally:
        for name in ("viewer", "agent", "hub"):
            if name in procs:
                terminate(procs[name], name)
        for f in log_files:
            try:
                f.close()
            except Exception:
                pass


if __name__ == "__main__":
    sys.exit(main())
