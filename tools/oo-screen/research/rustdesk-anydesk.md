# RustDesk vs AnyDesk — transport core (32 params)

RustDesk pinned: RS = https://github.com/rustdesk/rustdesk/blob/e5bc204fe4dacc4db9c3cdb1f1338813986c89e7 ; HC = https://github.com/rustdesk/hbb_common/blob/94ec710894ecd548b4fe174ef781eca5c1a318c0
AnyDesk: KB = https://support.anydesk.com/docs/ ; anydesk.com/en/* returned 403 to fetches (cells from it are marked "(snippet)").

| # | Parameter | RustDesk | src | перевірено RD | AnyDesk | src | перевірено AD |
|---|---|---|---|---|---|---|---|
| 1 | Transport protocol | Own protobuf protocol over TCP + UDP (FramedSocket); optional KCP stream; WebRTC (webrtc-rs data channels, `webrtc` cargo feature, raced vs punch/relay) | RS/src/rendezvous_mediator.rs, RS/src/kcp_stream.rs, HC/src/webrtc.rs | так | Proprietary; details не знайдено | — | ні |
| 2 | P2P vs relay / NAT | UDP punch preferred, TCP punch fallback, relay (hbbr) fallback; WebRTC ICE attempt | RS/src/rendezvous_mediator.rs | так | P2P + relay (snippet) | https://anydesk.com/en/performance | ні (snippet) |
| 3 | Encryption | sodiumoxide: Ed25519 sign (identity), box_ (Curve25519) key exchange, secretbox (XSalsa20-Poly1305) stream | RS/src/client.rs, HC/src/tcp.rs | так | TLS 1.2 with AEAD (KB); RSA-2048 exchange, AES-256, PFS (snippet) | KB/security-and-privacy.md ; https://anydesk.com/en/security | частково (TLS 1.2 AEAD так; rest snippet) |
| 4 | Auth model | ID + temporary/permanent password, signed server key | RS/src/client.rs, HC/src/config.rs | частково | ID + accept or Unattended Access password; 2FA for unattended | KB/unattended-access.md, KB/require-2fa-for-unattended-access-custom-clients.md | так (page titles) |
| 5 | Video codecs | VP8, VP9 (default), AV1, H.264, H.265 | RS/libs/scrap/src/common/mod.rs (enum CodecFormat), codec.rs | так | DeskRT proprietary (snippet) | https://anydesk.com/en/performance | ні (snippet) |
| 6 | 4:4:4 | Yes (`i444` param to encoders) | RS/libs/scrap/src/common/codec.rs | так | не знайдено | — | — |
| 7 | HDR/10-bit | не знайдено | — | — | не знайдено | — | — |
| 8 | Max resolution | не знайдено | — | — | не знайдено | — | — |
| 9 | Max FPS | 120 (MAX_FPS), default 30, init 15 | RS/src/server/video_qos.rs | так | 60 fps (snippet) | https://anydesk.com/en/performance | ні (snippet) |
| 10 | Published latency | не знайдено | — | — | <16 ms LAN (snippet) | https://anydesk.com/en/performance | ні (snippet) |
| 11 | Rate control | Delay-based QoS (TestDelay RTT, 150 ms threshold), adjusts fps + bitrate ratio every 3 s | RS/src/server/video_qos.rs | так | Quality modes Best quality / Balanced / Best performance; algorithm не знайдено | KB/display.md | частково |
| 12 | Bitrate range | Ratio 0.2 (0.1 high-res) … 40 of base | RS/src/server/video_qos.rs | так | Works at 100 kB/s (snippet) | https://anydesk.com/en/performance | ні (snippet) |
| 13 | FEC | не знайдено (none found in source) | — | — | не знайдено | — | — |
| 14 | Loss recovery | Relies on reliable transport (TCP/KCP/SCTP data channel); specifics не знайдено | RS/src/kcp_stream.rs | частково | не знайдено | — | — |
| 15 | Jitter buffer/pacing | не знайдено | — | — | не знайдено | — | — |
| 16 | HW encoders | hwcodec (FFmpeg-based: VAAPI, VideoToolbox, MediaCodec named), VRAM path with AMF + others via Driver enum; NVENC/QSV not named in opened files | RS/libs/scrap/src/common/hwcodec.rs, vram.rs | частково | не знайдено (Direct3D/DirectDraw/OpenGL listed are render, not encode) | KB/display.md | — |
| 17 | SW fallback | Falls back to VP9 (libvpx) when HW encoder fails | RS/libs/scrap/src/common/codec.rs | так | не знайдено | — | — |
| 18 | Capture API | DXGI (Win), Quartz (mac), X11, Wayland, DRM (feature), Android | RS/libs/scrap/src/common/mod.rs | так | не знайдено | — | — |
| 19 | Dirty-rect/static | WouldBlock = no new frame -> skip encode; explicit dirty-rect не знайдено | RS/src/server/video_service.rs | частково | Sends image changes (snippet) | https://anydesk.com/en/performance | ні (snippet) |
| 20 | Cursor separate layer | Yes: CursorData / CursorPosition messages | RS/src/server/connection.rs | так | Remote cursor modes hide/always/on movement | KB/display.md | так |
| 21 | Multi-monitor | Yes | RS/src/server/display_service.rs | так | Yes, Displays menu, separate windows (9.5+) | KB/display.md | так |
| 22 | HiDPI | не знайдено | — | — | Shrink/stretch scaling only; HiDPI не знайдено | KB/display.md | частково |
| 23 | Audio codec | Opus (magnum_opus) | RS/src/server/audio_service/audio_capture_encoder.rs | так | не знайдено | — | — |
| 24 | Mic passthrough | Yes, voice call (mic capture) | RS/src/server/connection.rs, audio_service/audio_capture_queue.rs | так | "transmit local audio and/or receive remote sound" | KB/settings.md, KB/audio.md | так |
| 25 | Input & transport | MouseEvent/KeyEvent/PointerDeviceEvent protobuf msgs on session connection; uinput on Linux | RS/src/server/connection.rs, input_service.rs, uinput.rs | так | не знайдено | — | — |
| 26 | Clipboard | Yes | RS/src/server/connection.rs | частково | Yes (Android page: clipboard sync) | KB/other-platforms.md | так |
| 27 | File transfer | Yes | RS/src/server/connection.rs | частково | Yes | KB/other-platforms.md | так |
| 28 | Client platforms | Win, macOS, Linux, Android, iOS, web | https://github.com/rustdesk/rustdesk/tree/e5bc204fe4dacc4db9c3cdb1f1338813986c89e7 | частково | Win, macOS, Linux/Raspberry Pi, Android/ChromeOS, iOS/iPadOS/tvOS; browser не знайдено | KB/other-platforms.md | так |
| 29 | Host platforms | Win (DXGI), macOS, Linux X11/Wayland, Android | RS/libs/scrap/src/common/mod.rs | так | Win, Win Server, macOS, Linux, Android | KB/other-platforms.md | так |
| 30 | Multi-viewer | Yes (QoS over all users) | RS/src/server/video_qos.rs | так | Concurrent sessions exist; multi-viewer не знайдено | KB/session-recording.md | частково |
| 31 | Session recording | Yes: Recorder, WebM (VPx/AV1) or MP4 (HW) | RS/libs/scrap/src/common/record.rs | так | Yes, .ANYDESK format, local only | KB/session-recording.md | так |
| 32 | License | AGPL-3.0 | RS/LICENCE | так | Proprietary | https://anydesk.com/en/security | ні (snippet) |
