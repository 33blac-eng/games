//go:build windows && wt

// Легасі-транспорт WebTransport (кандидат B, T1/T2-бенч): envelope-кадри по
// QUIC-стріму до hub-wt. У прод-бінар НЕ входить — він вимикає перевірку TLS
// (самопідписаний сертифікат стенду), а прод ходить лише WebRTC. Бенч
// збирає агента з -tags wt.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/organicoils/oo-screen/agent/encode"
	"github.com/organicoils/oo-screen/internal/control"
	"github.com/organicoils/oo-screen/internal/envelope"
)

const (
	agentALPN = "oo-screen-agent"
	// wtWriteTimeout — стеля на ОДИН запис кадру в QUIC-стрім (A-33).
	//
	// Не «скільки не шкода чекати», а «з якої миті чекати вже нема сенсу»:
	// admissionFloor — найдовша пауза, яку ми взагалі дозволяємо потоку (два
	// keepaliveAfter, далі сторож у браузері рве сесію). Запис, що не вклався в
	// неї, вже нічого не рятує — картинка на тому боці однаково прострочена,
	// тож дешевше визнати транспорт мертвим і перепідключитись.
	wtWriteTimeout = admissionFloor
)

// ---- WebTransport (кандидат B): envelope-кадри по QUIC-стріму -------------

type wtTransport struct {
	conn     *quic.Conn
	videoStr *quic.Stream
}

func dialWT(hubAddr string, onKeyframeRequest func(), onBitrateTarget func(uint64), onSelectOutput func(int)) (transport, error) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, // T1/T2: самопідписаний сертифікат hub-wt
		NextProtos:         []string{agentALPN},
	}
	dialCtx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	conn, err := quic.DialAddr(dialCtx, hubAddr, tlsConf, &quic.Config{})
	if err != nil {
		return nil, fmt.Errorf("dial hub-wt %s: %w", hubAddr, err)
	}

	ctrlStr, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open control stream: %w", err)
	}
	if err := control.Write(ctrlStr, control.Hello(authToken(), 1)); err != nil {
		conn.CloseWithError(0, "")
		return nil, fmt.Errorf("send hello: %w", err)
	}
	// heartbeat раз/5с, поки конект живий (control-протокол §5.4)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		var seq uint64 = 1
		for range t.C {
			seq++
			if err := control.Write(ctrlStr, control.Heartbeat(seq)); err != nil {
				return
			}
		}
	}()

	// Control-стрім двонаправлений: hub шле keyframe_request сюди ж, поки
	// агент лише пише (heartbeat) і ніколи не читає — запити зависають у
	// буфері й ForceIDR ніколи не викликається. Читаємо персистентно й на
	// keyframe_request віддаємо колбек у main() (§5.4/§5.5).
	go func() {
		br := bufio.NewReader(ctrlStr)
		for {
			m, err := control.ReadKnown(br, nil)
			if err != nil {
				return // конект/стрім мертвий — reconnect-логіка в main() це побачить через send-помилки
			}
			if m.Type == control.TypeKeyframeRequest && onKeyframeRequest != nil {
				onKeyframeRequest()
			}
			if m.Type == control.TypeBitrateTarget && onBitrateTarget != nil {
				onBitrateTarget(m.BitrateBps)
			}
			// Дзеркало WebRTC-гілки (handleCtlMessage): вибір монітора мусить
			// працювати обома ногами, інакше «перемкни екран» тихо не діяло б
			// саме на тому транспорті, яким знімають бенчі.
			if m.Type == control.TypeSelectOutput && onSelectOutput != nil {
				onSelectOutput(m.Output)
			}
			if m.Type == control.TypeMaxFps {
				setMaxFps(m.Fps)
			}
		}
	}()

	videoStr, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open video stream: %w", err)
	}

	return &wtTransport{conn: conn, videoStr: videoStr}, nil
}

func (t *wtTransport) send(au encode.AU, seq uint64) error {
	flags := uint8(0)
	if au.Keyframe {
		flags |= envelope.FlagKeyframe
		// Енкодер (agent/encode) вставляє SPS/PPS у кожен IDR — контракт §5.2.
		flags |= envelope.FlagConfigured
	}
	f := &envelope.Frame{
		Flags: flags,
		// Епоха БІЛЬШЕ НЕ КОНСТАНТА: SwitchOutput зсуває її, бо інший монітор —
		// інша геометрія, тобто інший SPS. Глядач мусить побачити зсув, інакше
		// нова геометрія прийде посеред старого потоку (див. output.go).
		ConfigEpoch: currentEpoch(),
		FrameSeq:    seq,
		PTS:         uint64(au.PTS.Microseconds()),
		Payload:     bytes.Clone(au.Data),
	}
	buf, err := f.Marshal()
	if err != nil {
		return fmt.Errorf("marshal frame seq=%d: %w", seq, err)
	}
	// 🚨 A-33. Без дедлайну Write на QUIC-стрімі блокується НАЗАВЖДИ, щойно
	// вікно flow control закрилось: хаб перестав вичитувати (завис, а не впав),
	// вікно не рухається — і єдиний ordered sender стоїть у цьому виклику. А
	// поки він стоїть, у txErrCh нічого не приходить, тобто реконект, який мав
	// би це полагодити, не запускається взагалі. Дедлайн перетворює зависання
	// на звичайну помилку відправки, а її кадровий цикл уже вміє лікувати.
	if err := t.videoStr.SetWriteDeadline(time.Now().Add(wtWriteTimeout)); err != nil {
		return fmt.Errorf("set write deadline seq=%d: %w", seq, err)
	}
	if _, err := t.videoStr.Write(buf); err != nil {
		return fmt.Errorf("write frame seq=%d: %w", seq, err)
	}
	return nil
}

// sendAudio: нога WT — бенчова, доріжок у ній немає взагалі (envelope возить
// самі AU відео). Звук туди не їде і ніколи не їхав; runAudio для цього
// транспорту й не стартує (main: гілка лише для webrtc).
func (t *wtTransport) sendAudio([]byte, time.Duration) error { return nil }

func (t *wtTransport) close() {
	if t.videoStr != nil {
		_ = t.videoStr.Close()
	}
	if t.conn != nil {
		_ = t.conn.CloseWithError(0, "")
	}
}
