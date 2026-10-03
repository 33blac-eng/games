// wt-worker.js — кандидат B (WebTransport). Приймає uni-stream з hub-wt,
// парсить envelope-кадри (Додаток D), конвертує Annex-B -> AVCC, годує
// WebCodecs VideoDecoder, рендерить у OffscreenCanvas, збирає метрики.

const ENVELOPE_MAGIC = 0x4f4f5343; // 'OOSC' big-endian read як u32
const HEADER_SIZE = 28;
const FLAG_KEYFRAME = 1 << 0;
const SUPPORTED_VERSION = 1;
const MAX_PAYLOAD_LEN = 8 * 1024 * 1024; // 8 МіБ

let canvasCtx = null;
let sessionFatal = false;
let lastCanvasWidth = -1;
let lastCanvasHeight = -1;
let decoder = null;
let sawFirstKeyframe = false;
let avcDescription = null; // AVCDecoderConfigurationRecord (Uint8Array)
let metrics = []; // {seq, t_arrival_ms, t_decoded_ms, t_rendered_ms, bytes, key}
let pendingBySeq = new Map(); // seq -> metric row, поки не decoded/rendered

// --- control-стрім (browser<->hub, bidi WebTransport стрім) ---
const CONTROL_VERSION = 1;
let controlWriter = null;
let controlToken = null;
let controlSeq = 0;
let heartbeatTimer = null;

function nextControlSeq() {
  controlSeq += 1;
  return controlSeq;
}

// sendControl шле один рядок line-delimited JSON control-повідомлення
// (Додаток "internal/control" на Go-стороні — той самий framing: JSON + \n).
function sendControl(type, extra) {
  if (!controlWriter) return;
  const msg = Object.assign({ v: CONTROL_VERSION, type, seq: nextControlSeq() }, extra || {});
  const bytes = new TextEncoder().encode(JSON.stringify(msg) + "\n");
  controlWriter.write(bytes).catch((e) => {
    self.postMessage({ type: "error", message: "control write failed: " + e });
  });
}

function initControlStream(writable, token) {
  controlToken = token || null;
  controlWriter = writable.getWriter();
  sendControl("hello", controlToken ? { token: controlToken } : undefined);
  if (heartbeatTimer) clearInterval(heartbeatTimer);
  heartbeatTimer = setInterval(() => sendControl("heartbeat"), 5000);
}

self.onmessage = async (ev) => {
  const msg = ev.data;
  if (msg.type === "init") {
    if (msg.canvas) {
      canvasCtx = msg.canvas.getContext("2d");
    }
    initDecoder();
  } else if (msg.type === "control-stream") {
    initControlStream(msg.writable, msg.token);
  } else if (msg.type === "stream") {
    consumeStream(msg.stream);
  } else if (msg.type === "download") {
    const blob = new Blob([metrics.map((m) => JSON.stringify(m)).join("\n") + "\n"], {
      type: "application/x-ndjson",
    });
    self.postMessage({ type: "metrics-blob", blob });
  }
};

// fatal закриває сесію: повідомляє глядача про помилку і зупиняє декодер,
// щоб worker не продовжував мовчки жувати потік після протокольної помилки.
function fatal(message) {
  if (sessionFatal) return;
  sessionFatal = true;
  self.postMessage({ type: "error", message, fatal: true });
  try {
    decoder && decoder.state !== "closed" && decoder.close();
  } catch (e) {
    // decoder вже міг бути в невалідному стані — ігноруємо
  }
}

function initDecoder() {
  decoder = new VideoDecoder({
    output: (frame) => {
      const row = pendingBySeq.get(frame.timestamp);
      if (row) {
        row.t_decoded_ms = performance.now();
      }
      if (canvasCtx) {
        try {
          // Backing store канвасу ресетиться при кожному присвоєнні
          // width/height навіть коли розмір не змінився — на 60fps це
          // зайва алокація на кожен кадр. Міняємо лише при реальній зміні.
          if (frame.displayWidth !== lastCanvasWidth || frame.displayHeight !== lastCanvasHeight) {
            canvasCtx.canvas.width = frame.displayWidth;
            canvasCtx.canvas.height = frame.displayHeight;
            lastCanvasWidth = frame.displayWidth;
            lastCanvasHeight = frame.displayHeight;
          }
          canvasCtx.drawImage(frame, 0, 0);
        } catch (e) {
          // ignore draw errors in worker без OffscreenCanvas
        }
      }
      const ts = frame.timestamp;
      frame.close();
      if (row) {
        // t_rendered_ms фіксуємо на наступному composite tick (rAF), а не
        // одразу після drawImage — так вимір найближчий до фактичної
        // презентації кадру і зіставний із кандидатом A, що міряє на
        // presentation callback. rVFC для canvas у worker недоступний
        // (план §10), тож rAF — найближча доступна межа.
        if (typeof requestAnimationFrame === "function") {
          requestAnimationFrame(() => {
            row.t_rendered_ms = performance.now();
            pendingBySeq.delete(ts);
          });
        } else {
          row.t_rendered_ms = performance.now();
          pendingBySeq.delete(ts);
        }
      }
    },
    error: (e) => {
      self.postMessage({ type: "error", message: String(e) });
      // decoder error/reset — просимо hub про свіжий keyframe, щоб
      // відновитись, а не чекати природний наступний GOP.
      sendControl("keyframe_request");
    },
  });
}

// consumeStream читає ReadableStream<Uint8Array> шматками, зшиває їх у
// суцільний буфер і виймає envelope-кадри по HeaderSize + payload_len.
async function consumeStream(stream) {
  const reader = stream.getReader();
  let buf = new Uint8Array(0);

  function append(chunk) {
    const n = new Uint8Array(buf.length + chunk.length);
    n.set(buf, 0);
    n.set(chunk, buf.length);
    buf = n;
  }

  while (true) {
    if (sessionFatal) return;
    const { value, done } = await reader.read();
    const tArrival = performance.now();
    if (done) break;
    if (value) append(value);

    while (buf.length >= HEADER_SIZE) {
      const dv = new DataView(buf.buffer, buf.byteOffset, buf.length);
      const magic = dv.getUint32(0, false);
      if (magic !== ENVELOPE_MAGIC) {
        fatal("bad envelope magic");
        buf = new Uint8Array(0);
        return;
      }
      const version = dv.getUint8(4);
      if (version !== SUPPORTED_VERSION) {
        fatal("unsupported envelope version: " + version);
        return;
      }
      const flags = dv.getUint8(5);
      const frameSeq = dv.getBigUint64(8, true);
      const pts = dv.getBigUint64(16, true);
      const payloadLen = dv.getUint32(24, true);
      if (payloadLen > MAX_PAYLOAD_LEN) {
        fatal("payload_len exceeds cap: " + payloadLen);
        return;
      }
      const total = HEADER_SIZE + payloadLen;
      if (buf.length < total) break; // чекаємо решту кадру

      const payload = buf.slice(HEADER_SIZE, total);
      buf = buf.slice(total);

      const seqNum = Number(frameSeq);
      const key = (flags & FLAG_KEYFRAME) !== 0;
      const row = { seq: seqNum, t_arrival_ms: tArrival, t_decoded_ms: null, t_rendered_ms: null, bytes: payload.length, key };
      metrics.push(row);

      handleFrame(payload, Number(pts), key, seqNum, row);
    }
  }
}

function handleFrame(annexB, ptsUs, isKey, seqNum, row) {
  if (!sawFirstKeyframe) {
    if (!isKey) return; // чекаємо перший keyframe перед стартом декоду
    sawFirstKeyframe = true;
    const { sps, pps } = findSpsPps(annexB);
    if (!sps || !pps) {
      self.postMessage({ type: "error", message: "no SPS/PPS in first keyframe" });
      return;
    }
    avcDescription = buildAvcDescription(sps, pps);
    decoder.configure({
      codec: "avc1.64002A",
      optimizeForLatency: true,
      description: avcDescription,
    });
    self.postMessage({ type: "started" });
    sendControl("decoder_ready");
  }

  const avcc = annexBToAvcc(annexB);
  // timestamp мусить бути унікальним і монотонним в мкс — використовуємо PTS
  // як ключ зіставлення VideoFrame.timestamp -> метрика цього кадру.
  pendingBySeq.set(ptsUs, row);

  const chunk = new EncodedVideoChunk({
    type: isKey ? "key" : "delta",
    timestamp: ptsUs,
    data: avcc,
  });
  try {
    decoder.decode(chunk);
  } catch (e) {
    // decode() кинув синхронно — output-колбек ніколи не спрацює для
    // цього кадру, тож запис у pendingBySeq інакше висить вічно.
    pendingBySeq.delete(ptsUs);
    self.postMessage({ type: "error", message: "decode failed: " + e });
    // decode() кинув синхронно — трактуємо як decoder reset і просимо
    // hub про свіжий keyframe для відновлення.
    sendControl("keyframe_request");
  }
}

// --- Annex-B parsing helpers ---

function findStartCodes(b) {
  const positions = [];
  for (let i = 0; i + 2 < b.length; i++) {
    if (b[i] === 0 && b[i + 1] === 0 && b[i + 2] === 1) {
      positions.push(i);
    }
  }
  return positions;
}

// findSpsPps шукає SPS (type 7) і PPS (type 8) NAL-юніти в Annex-B AU
// через сканування стартових кодів (без парсингу semantics — лише байти).
function findSpsPps(annexB) {
  const starts = findStartCodes(annexB);
  let sps = null;
  let pps = null;
  for (let i = 0; i < starts.length; i++) {
    const nalStart = starts[i] + 3;
    const nalEnd = i + 1 < starts.length ? trimStartCode(starts[i + 1], annexB) : annexB.length;
    if (nalStart >= annexB.length) continue;
    const type = annexB[nalStart] & 0x1f;
    const nal = annexB.slice(nalStart, nalEnd);
    if (type === 7 && !sps) sps = nal;
    if (type === 8 && !pps) pps = nal;
  }
  return { sps, pps };
}

function trimStartCode(nextStart, b) {
  // 4-байтний стартовий код: попередній байт 0 не входить у попередній NAL.
  if (nextStart > 0 && b[nextStart - 1] === 0) return nextStart - 1;
  return nextStart;
}

// annexBToAvcc конвертує Annex-B AU (стартові коди 00 00 01 / 00 00 00 01)
// у AVCC: кожен NAL передується 4-байтною довжиною (big-endian).
function annexBToAvcc(annexB) {
  const starts = findStartCodes(annexB);
  const nals = [];
  let total = 0;
  for (let i = 0; i < starts.length; i++) {
    const nalStart = starts[i] + 3;
    const nalEnd = i + 1 < starts.length ? trimStartCode(starts[i + 1], annexB) : annexB.length;
    if (nalStart >= nalEnd) continue;
    const nal = annexB.slice(nalStart, nalEnd);
    nals.push(nal);
    total += 4 + nal.length;
  }
  const out = new Uint8Array(total);
  let off = 0;
  for (const nal of nals) {
    const dv = new DataView(out.buffer);
    dv.setUint32(off, nal.length, false);
    out.set(nal, off + 4);
    off += 4 + nal.length;
  }
  return out;
}

// buildAvcDescription будує мінімальний AVCDecoderConfigurationRecord
// (ISO 14496-15) з одного SPS і одного PPS для WebCodecs "avc" опису.
function buildAvcDescription(sps, pps) {
  const profileIdc = sps[1];
  const compatFlags = sps[2];
  const levelIdc = sps[3];
  const parts = [];
  parts.push(new Uint8Array([
    1, // configurationVersion
    profileIdc,
    compatFlags,
    levelIdc,
    0xff, // reserved(6)=111111 + lengthSizeMinusOne=11 (4-байтна довжина)
    0xe1, // reserved(3)=111 + numOfSequenceParameterSets=00001
  ]));
  const spsLen = new Uint8Array(2);
  new DataView(spsLen.buffer).setUint16(0, sps.length, false);
  parts.push(spsLen, sps);
  parts.push(new Uint8Array([1])); // numOfPictureParameterSets
  const ppsLen = new Uint8Array(2);
  new DataView(ppsLen.buffer).setUint16(0, pps.length, false);
  parts.push(ppsLen, pps);

  let total = 0;
  for (const p of parts) total += p.length;
  const out = new Uint8Array(total);
  let off = 0;
  for (const p of parts) {
    out.set(p, off);
    off += p.length;
  }
  return out;
}
