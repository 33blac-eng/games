#!/usr/bin/env bash
# netem.sh — impairment profiles S2..S7 (Додаток A) for oo-screen T1 hub.
#
# RUNS ON THE VPS ONLY (shared prod box: ERP + teplokram + jobhunt).
# Touches ONLY UDP ports 4460 (hub-wt QUIC ingest) and 4461 (hub-wt
# WebTransport) via a classful qdisc + u32 filter match — every other
# packet on the interface falls through to the default class UNCHANGED.
# Never run `tc qdisc add dev <if> root netem ...` directly: that would
# impair the WHOLE interface (ERP/teplokram/jobhunt/SSH included).
#
# *** `clear` IS MANDATORY AFTER EVERY RUN. *** An impairment left
# applied silently keeps degrading oo-screen traffic (and, if you ever
# widen the filter, other services) after the bench finishes.
#
# Profiles S2-S7 are the Windows-clumsy-can't-do trace set from Додаток A
# (packet-level netem effects that clumsy's simple loss/lag model cannot
# reproduce faithfully): gemodel bursty loss, reorder-with-gap, staged
# rate collapse, etc. S2-S7 exactly per Dodatok A (delay = full RTT applied on egress class only; ingress clean) —
# VERIFY THEIR EXACT PARAMETERS AGAINST THE Додаток A TEXT before using
# results for the report; only S4/S5/S7 were given explicit numbers.
#
# Usage (run as root on the VPS):
#   ./netem.sh apply S2   # ... through S7
#   ./netem.sh s7-start   # stage 1 of S7 (12mbit)
#   ./netem.sh s7-collapse   # stage 2 of S7 (drops to 2mbit)
#   ./netem.sh status
#   ./netem.sh clear      # ALWAYS run this when done

set -euo pipefail

IFACE="${OO_NETEM_IFACE:-ens3}"
PORTS="4460 4461"
# Candidate A (hub-webrtc) RTP uses pion's ephemeral UDP port range, fixed to
# 4544-4607 in hub/cmd/hub-webrtc/main.go (SetEphemeralUDPPortRange). This
# range is aligned to a 64-port block so a single u32 mask 0xffc0 covers it
# in one filter pair (dport+sport) instead of 64 individual filters.
EPHEMERAL_BASE="4544"
EPHEMERAL_MASK="0xffc0"
ROOT_HANDLE="1:"
IMPAIRED_CLASSID="1:10"
DEFAULT_CLASSID="1:1"
IMPAIRED_QDISC="10:"

usage() {
  echo "usage: $0 {apply S2|S3|S4|S5|S6|S7|s7-start|s7-collapse|clear|status}" >&2
  exit 1
}

teardown() {
  tc qdisc del dev "$IFACE" root 2>/dev/null || true
}

setup_base() {
  teardown
  # htb root: class 1:1 = default (untouched traffic), 1:10 = impaired class
  tc qdisc add dev "$IFACE" root handle 1: htb default 1
  tc class add dev "$IFACE" parent 1: classid "$DEFAULT_CLASSID" htb rate 1000mbit
  tc class add dev "$IFACE" parent 1: classid "$IMPAIRED_CLASSID" htb rate 1000mbit
  for p in $PORTS; do
    tc filter add dev "$IFACE" parent 1: protocol ip prio 1 u32 \
      match ip dport "$p" 0xffff match ip protocol 17 0xff flowid "$IMPAIRED_CLASSID"
    tc filter add dev "$IFACE" parent 1: protocol ip prio 1 u32 \
      match ip sport "$p" 0xffff match ip protocol 17 0xff flowid "$IMPAIRED_CLASSID"
  done
  # Candidate A ephemeral RTP range (4544-4607) — one filter pair via mask.
  tc filter add dev "$IFACE" parent 1: protocol ip prio 1 u32 \
    match ip dport "$EPHEMERAL_BASE" "$EPHEMERAL_MASK" match ip protocol 17 0xff flowid "$IMPAIRED_CLASSID"
  tc filter add dev "$IFACE" parent 1: protocol ip prio 1 u32 \
    match ip sport "$EPHEMERAL_BASE" "$EPHEMERAL_MASK" match ip protocol 17 0xff flowid "$IMPAIRED_CLASSID"
}

apply_netem() {
  # $1 = netem args appended after "tc qdisc add ... netem"
  tc qdisc del dev "$IFACE" parent "$IMPAIRED_CLASSID" handle "$IMPAIRED_QDISC" 2>/dev/null || true
  # shellcheck disable=SC2086
  tc qdisc add dev "$IFACE" parent "$IMPAIRED_CLASSID" handle "$IMPAIRED_QDISC" netem $1
}

apply_profile() {
  case "$1" in
    S2)
      setup_base
      apply_netem "delay 20ms loss 1%"   # S2: 1% loss, RTT 20ms (Dodatok A)
      ;;
    S3)
      setup_base
      apply_netem "delay 60ms loss 3%"   # S3: 3% loss, RTT 60ms (Dodatok A)
      ;;
    S4)
      setup_base
      apply_netem "delay 60ms loss gemodel 5% 40%"   # S4: 5% GE-burst, RTT 60ms
      ;;
    S5)
      setup_base
      apply_netem "delay 60ms reorder 2% gap 5"   # S5: 2% reorder gap5, RTT 60ms
      ;;
    S6)
      setup_base
      apply_netem "delay 120ms loss 0.5%"   # S6: RTT 120ms, 0.5% loss (Dodatok A)
      ;;
    S7)
      s7_start
      ;;
    *)
      echo "unknown profile: $1" >&2; exit 1 ;;
  esac
  echo "applied $1 on $IFACE (udp ports: $PORTS, ephemeral A range: 4544-4607/0xffc0)"
}

s7_start() {
  setup_base
  tc class change dev "$IFACE" parent 1: classid "$IMPAIRED_CLASSID" htb rate 12mbit ceil 12mbit
  echo "S7 stage 1: 12mbit on impaired class. Run '$0 s7-collapse' to drop to 2mbit."
}

s7_collapse() {
  tc class change dev "$IFACE" parent 1: classid "$IMPAIRED_CLASSID" htb rate 2mbit ceil 2mbit
  echo "S7 stage 2: collapsed to 2mbit on impaired class."
}

clear_all() {
  teardown
  echo "cleared. tc qdisc show dev $IFACE:"
  tc qdisc show dev "$IFACE"
}

status() {
  echo "--- qdisc ($IFACE) ---"
  tc -s qdisc show dev "$IFACE"
  echo "--- class ($IFACE) ---"
  tc -s class show dev "$IFACE"
  echo "--- filter ($IFACE) ---"
  tc filter show dev "$IFACE"
}

cmd="${1:-}"
case "$cmd" in
  apply)
    [ -n "${2:-}" ] || usage
    apply_profile "$2"
    ;;
  s7-start) s7_start ;;
  s7-collapse) s7_collapse ;;
  clear) clear_all ;;
  status) status ;;
  *) usage ;;
esac
