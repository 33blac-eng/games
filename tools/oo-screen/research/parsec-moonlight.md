# Parsec vs Moonlight/Sunshine — transport core (verified 2026-10-03)

"не знайдено" = no opened primary source. "(snippet)" = only search-engine snippet, page not opened. "перевірено" = ✅ value checked against an opened page / source file; ⚠️ partial/snippet; ❌ not verified.

Sources:
- P1 https://parsec.app/technology
- P2 https://parsec.app/blog/a-networking-protocol-built-for-the-lowest-latency-interactive-game-streaming-1fd5a03a6007/
- P3 Feature Matrix https://support.parsec.app/hc/en-us/articles/4407194154253 (read via web.archive.org 2025 snapshot)
- P4 All Advanced Configuration Options https://support.parsec.app/hc/en-us/articles/360001562772 (via web.archive.org, updated 2025-09-02)
- P5 Microphone https://support.parsec.app/hc/en-us/articles/32380350695956 (via web.archive.org)
- S1 https://docs.lizardbyte.dev/projects/sunshine/latest/md_docs_2configuration.html
- Source code (shallow clones of default branch, 2026-10-03): MC = github.com/moonlight-stream/moonlight-common-c, SU = github.com/LizardByte/Sunshine, MQ = github.com/moonlight-stream/moonlight-qt

| # | Parameter | Parsec | src | перевірено | Moonlight/Sunshine | src | перевірено |
|---|---|---|---|---|---|---|---|
| 1 | Transport protocol | BUD (Better User Datagrams), proprietary, UDP-based, TCP-like reliability | P1, P2 | ✅ | RTSP handshake + ENet (UDP) control stream + RTP/UDP video & audio; HTTP(S) pairing on base port 47989 (HTTPS 47984) | MC src/RtspConnection.c, src/ControlStream.c (enet_peer_send), src/RtpVideoQueue.c; SU src/config.cpp:883 | ✅ |
| 2 | P2P / relay / NAT | P2P, aggressive hole punching + optional client UPnP, 97% traversal claim; Parsec Relay Server (Teams/Enterprise, `app_stun_address`, `app_force_relay`) | P1, P2, P3, P4 | ✅ | No relay/STUN in code; direct connection, ports configurable (S1) | S1 | ⚠️ (relay absence inferred) |
| 3 | Encryption | DTLS 1.2 (OpenSSL), AES-128 or AES-256 per packet | P1, P2 | ✅ | AES-128-GCM (16-byte tag) on control stream, optional audio/video encryption (ENCFLG_AUDIO/VIDEO); defaults LAN=never, WAN=opportunistic | MC src/ControlStream.c:25,585, src/Limelight.h:33-36; SU src/config.cpp:822-823 | ✅ |
| 4 | Auth model | Parsec account; owner vs guest; SAML SSO (Warp/Teams); mic needs same account | P3, P5 | ⚠️ | PIN pairing with X.509 client certificates, stored and compared on host | SU src/nvhttp.cpp:301-337 | ✅ |
| 5 | Video codecs | H.264, H.265 (fallback to H.264) | P3, P4 | ✅ | H.264, HEVC, AV1 | SU src/video.cpp (profile_av1_e); MQ SettingsView.qml:1647 | ✅ |
| 6 | 4:4:4 | Yes, Windows host only (not macOS); client decode on Win/mac/Ubuntu; needs Warp/Teams | P3, P4 | ✅ | YUV444 code paths present | SU src/video.h, src/platform/common.h | ⚠️ (presence only) |
| 7 | HDR / 10-bit | 10-bit (`client_decoder_10bit`, HEVC only); HDR не знайдено | P4 | ✅ | HDR supported (config auto-detect) | S1 | ⚠️ |
| 8 | Max resolution | Host resolution switching must be 4K or lower | P4 | ✅ | не знайдено (no hard cap found) | — | ❌ |
| 9 | Max FPS | `encoder_fps=60` default, >60 "may cause instability"; max не знайдено | P4 | ⚠️ | не знайдено | — | ❌ |
| 10 | Published latency | "adds only 7 ms" (LAN ethernet) | P1 | ✅ | не знайдено | — | — |
| 11 | Rate / congestion control | Custom tuned congestion control, dynamic bitrate | P1, P2 | ✅ | Client-chosen bitrate, host cap `max_bitrate`; adaptive CC не знайдено | S1, SU src/config.cpp:1734 | ⚠️ |
| 12 | Bitrate range | `encoder_bitrate=10` Mbps default, shared by all viewers; `encoder_min_qp=5`; max не знайдено | P4 | ✅ | Client slider 0.5–150 Mbps (500 Mbps when unlocked), step 0.5 | MQ app/gui/SettingsView.qml:690-697 | ✅ |
| 13 | FEC | не знайдено | — | — | Reed-Solomon (nanors) FEC, `fec_percentage` default 20, range 1–255 | MC nanors/, src/RtpVideoQueue.c; SU src/config.cpp:820,1805 | ✅ |
| 14 | Loss recovery | Reliability semantics in BUD; details не знайдено | P2 | ⚠️ | FEC + reference frame invalidation (IDX_INVALIDATE_REF_FRAMES) / IDR request | MC src/ControlStream.c:97,133,1510 | ✅ |
| 15 | Jitter buffer / pacing | не знайдено (only client VSync option) | P4 | — | не знайдено | — | — |
| 16 | HW encoders | NVIDIA & AMD (P1); hardware encoder required (snippet) | P1 | ⚠️ | NVENC, QuickSync, AMF, VA-API, VideoToolbox, Vulkan | S1 | ✅ |
| 17 | SW fallback | не знайдено (snippet: host needs HW encoder) | — | ❌ | Software encoder (presets ultrafast…veryslow) | S1 | ✅ |
| 18 | Capture API | не знайдено | — | — | Win: DXGI Desktop Duplication, WGC; Linux: NvFBC, KMS, X11, wlroots, KWin, portal/PipeWire; macOS: AVCaptureScreenInput | S1; SU src/platform/macos/av_video.m:31, src/platform/linux/portalgrab.cpp | ✅ |
| 19 | Dirty-rect / static | не знайдено | — | — | Minimum-framerate option for static content (S1); dirty-rect не знайдено | S1 | ⚠️ |
| 20 | Cursor | Virtual mouse option keeps cursor visible (VUSB); separate layer не знайдено | P4 | ⚠️ | не знайдено | — | — |
| 21 | Multi-monitor | Multi-monitor streaming, up to 3 screens; extra screens need Warp/Teams | P3, P4 | ✅ | Single output per stream (`output_name`) | SU src/config.cpp:1711 | ⚠️ |
| 22 | HiDPI / scaling | не знайдено | — | — | не знайдено | — | — |
| 23 | Audio | Audio streaming (Win host needs Parsec Audio Capture Driver); codec не знайдено | P3 | ⚠️ | Opus multistream, stereo / 5.1 / 7.1 surround | SU src/audio.cpp:9,89-120 | ✅ |
| 24 | Mic passthrough | Yes (≥150-90), Win/macOS client → Windows host, VUSB driver ≥0.2.5.0 | P5 | ✅ | не знайдено (no mic uplink in MC) | MC (grep: no matches) | ⚠️ |
| 25 | Input | KB/mouse, gamepad (Windows host), pen (Warp/Teams), DS4 touchpad | P3 | ✅ | KB/mouse/gamepad, touch & pen (LiSendTouchEvent/LiSendPenEvent) over the ENet control stream | MC src/Limelight.h:581-670, src/InputStream.c | ✅ |
| 26 | Clipboard | (snippet) text clipboard sync | — | ❌ | No sync; moonlight-qt only types clipboard text as keystrokes | MQ app/streaming/input/keyboard.cpp:107-114 | ✅ |
| 27 | File transfer | не знайдено | — | — | не знайдено | — | — |
| 28 | Client platforms | Windows, macOS, Ubuntu, Android, Web (Chromium); web client = custom WebRTC | P1, P3 | ✅ | Moonlight-qt: Win/macOS/Linux; other clients не перевірено | MQ | ⚠️ |
| 29 | Host platforms | Windows, macOS | P3 | ✅ | Windows, Linux, macOS | SU src/platform/{windows,linux,macos} | ✅ |
| 30 | Multi-viewer | Multiple guests; bandwidth limit shared by all; link sharing | P3, P4 | ✅ | не знайдено | — | — |
| 31 | Session recording | не знайдено | — | — | не знайдено | — | — |
| 32 | Open source / license | Proprietary | P1 | ⚠️ | GPLv3 (Sunshine, moonlight-qt, moonlight-common-c) | LICENSE files in SU, MQ, MC | ✅ |
