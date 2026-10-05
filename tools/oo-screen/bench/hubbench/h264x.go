package main

// Розбір RTP-payload H.264 на боці глядача-вимірювача. Глядач НЕ декодує:
// йому треба лише знати, коли в нього вперше зібрався декодований старт
// (SPS + PPS + повний IDR без дірок у seq) — це і є «перший кадр».

// nalTypes повертає типи NAL, що несе RTP-payload (RFC 6184: одиничний NAL,
// STAP-A, FU-A). Для FU-A — тип фрагментованого NAL і чи це перший фрагмент.
func nalTypes(p []byte) (types []byte, fuStart bool) {
	if len(p) == 0 {
		return nil, false
	}
	switch t := p[0] & 0x1F; t {
	case 24: // STAP-A
		for i := 1; i+2 < len(p); {
			n := int(p[i])<<8 | int(p[i+1])
			i += 2
			if n == 0 || i+n > len(p) {
				break
			}
			types = append(types, p[i]&0x1F)
			i += n
		}
		return types, false
	case 28: // FU-A
		if len(p) < 2 {
			return nil, false
		}
		return []byte{p[1] & 0x1F}, p[1]&0x80 != 0
	default:
		return []byte{t}, false
	}
}

// idrTracker — чи зібрався в глядача перший декодований ключовий кадр.
// Правило: від пакета з SPS має бути безперервна послідовність seq, у ній
// PPS і IDR, і пакет із marker-бітом того ж timestamp, що й IDR, закриває AU.
// Будь-яка дірка в seq скидає збирання до наступного SPS.
type idrTracker struct {
	active   bool
	lastSeq  uint16
	sps, pps bool
	idr      bool
	idrTS    uint32
	done     bool
}

// feed повертає true рівно на тому пакеті, яким завершився перший повний IDR.
func (t *idrTracker) feed(seq uint16, ts uint32, marker bool, payload []byte) bool {
	if t.done {
		return false
	}
	if t.active && seq != t.lastSeq+1 {
		*t = idrTracker{}
	}
	types, _ := nalTypes(payload)
	for _, ty := range types {
		switch ty {
		case 7:
			if !t.active || t.idr {
				*t = idrTracker{active: true}
			}
			t.sps = true
		case 8:
			if t.active {
				t.pps = true
			}
		case 5:
			if t.active && t.sps && t.pps {
				if !t.idr {
					t.idrTS = ts
				}
				t.idr = true
			}
		}
	}
	if !t.active {
		return false
	}
	t.lastSeq = seq
	if t.idr && marker && ts == t.idrTS {
		t.done = true
		return true
	}
	return false
}

// isKeyStart — пакет починає ключовий набір (SPS) — для детектора
// «потік відновився з IDR» після реконекту агента.
func isKeyStart(p []byte) bool {
	types, _ := nalTypes(p)
	for _, t := range types {
		if t == 7 {
			return true
		}
	}
	return false
}
