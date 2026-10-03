package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/organicoils/oo-screen/internal/tiles"
	"github.com/pion/webrtc/v4"
)

func withTilesFlag(t *testing.T, on bool) {
	t.Helper()
	prevFlag, prevReg := tilesEnabled, reg
	tilesEnabled, reg = on, newRegistry()
	t.Cleanup(func() { tilesEnabled, reg = prevFlag, prevReg })
}

func tileMsg(t *testing.T, epoch uint32, x uint16, size int) []byte {
	t.Helper()
	b, err := tiles.Encode(&tiles.Msg{Type: tiles.TypeTile, Epoch: epoch, X: x, W: 64, H: 64,
		SrcW: 1920, SrcH: 1080, Format: tiles.FormatPNG, Payload: tilePNG(size)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tileAt(t *testing.T, epoch uint32, x, y uint16, tag byte) []byte {
	t.Helper()
	p := tilePNG(40)
	p[39] = tag
	b, err := tiles.Encode(&tiles.Msg{Type: tiles.TypeTile, Epoch: epoch, X: x, Y: y, W: 64, H: 64,
		SrcW: 1920, SrcH: 1080, Format: tiles.FormatPNG, Payload: p})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func feed(c *tilesCache, raw []byte) [][]byte {
	m, err := tiles.Decode(raw)
	if err != nil {
		panic(err)
	}
	var kr []tiles.Rect
	if m.Type == tiles.TypeKeep {
		kr = tiles.KeepRects(m.Payload)
	}
	return c.remember(m, raw, kr)
}

// summary — (type, epoch, x, tag) кожного повідомлення знімка.
func summary(t *testing.T, s [][]byte) []string {
	t.Helper()
	var out []string
	for _, raw := range s {
		m, err := tiles.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		switch m.Type {
		case tiles.TypeInvalidate:
			out = append(out, fmt.Sprintf("inv%d", m.Epoch))
		case tiles.TypeKeep:
			out = append(out, fmt.Sprintf("keep%d:%d", m.Epoch, len(tiles.KeepRects(m.Payload))))
		default:
			out = append(out, fmt.Sprintf("t%d@%d#%d", m.Epoch, m.X, m.Payload[39]))
		}
	}
	return out
}

func TestTilesCacheEpochs(t *testing.T) {
	var c tilesCache
	if c.snapshot() != nil {
		t.Fatal("empty cache replays")
	}
	feed(&c, tileAt(t, 1, 0, 0, 1))
	feed(&c, tileAt(t, 1, 64, 0, 2))
	if got := fmt.Sprint(summary(t, c.snapshot())); got != "[keep1:0 t1@0#1 t1@64#2]" {
		t.Fatalf("cache %s", got)
	}
	feed(&c, tiles.Invalidate(2, 0))
	if got := fmt.Sprint(summary(t, c.snapshot())); got != "[inv2]" {
		t.Fatalf("after invalidate %s", got)
	}
	// keep: тайл 0 лишається (перештампований у епоху 2), 64 — викинуто
	keep, _ := tiles.Keep(2, 0, 1920, 1080, []tiles.Rect{{X: 0, W: 64, H: 64}, {X: 128, W: 64, H: 64}})
	repair := feed(&c, keep)
	if got := fmt.Sprint(summary(t, repair)); got != "[t2@0#1]" {
		t.Fatalf("repair %s", got)
	}
	feed(&c, tileAt(t, 2, 192, 0, 3))
	if got := fmt.Sprint(summary(t, c.snapshot())); got != "[inv2 keep2:0 t2@0#1 t2@192#3]" {
		t.Fatalf("after keep %s", got)
	}
	// новий епізод без keep (старий агент): старі тайли недійсні
	feed(&c, tiles.Invalidate(3, 0))
	feed(&c, tileAt(t, 3, 64, 0, 4))
	if got := fmt.Sprint(summary(t, c.snapshot())); got != "[inv3 keep3:0 t3@64#4]" {
		t.Fatalf("no-keep episode %s", got)
	}
	// keep іншої геометрії стирає все
	feed(&c, tiles.Invalidate(4, 0))
	keep2, _ := tiles.Keep(4, 0, 1280, 1024, []tiles.Rect{{X: 64, W: 64, H: 64}})
	if r := feed(&c, keep2); len(r) != 0 || len(c.held) != 0 {
		t.Fatalf("geometry change kept %d", len(c.held))
	}
	big := make([]byte, tilesCacheBytes+1)
	c.remember(&tiles.Msg{Type: tiles.TypeTile, Epoch: 4, SrcW: 1280, SrcH: 1024, W: 64, H: 64}, big, nil)
	if len(c.held) != 0 {
		t.Fatal("cache cap ignored")
	}
	inv := c.reset()
	if m, err := tiles.Decode(inv); err != nil || m.Type != tiles.TypeInvalidate || len(c.snapshot()) != 0 {
		t.Fatalf("reset %v %v", m, err)
	}
	if c.reset() != nil {
		t.Fatal("reset of empty cache must not broadcast")
	}
}

// Глядач, що губив тайли, разом із keep-ом отримує перелічені тайли з кешу;
// глядач без втрат — лише keep.
func TestKeepRepairsLossyViewer(t *testing.T) {
	ns := &nodeSession{nodeID: "tiles-keep"}
	a, b := addViewer(ns, newPC(t), newViewerTrack(t), "u1"), addViewer(ns, newPC(t), newViewerTrack(t), "u2")
	t.Cleanup(func() { removeViewer(ns, a); removeViewer(ns, b) })
	ns.mu.Lock()
	a.tilesOut, b.tilesOut = make(chan []byte, 1), make(chan []byte, 8)
	ns.mu.Unlock()
	onAgentTiles(ns, tileAt(t, 1, 0, 0, 1))
	onAgentTiles(ns, tileAt(t, 1, 64, 0, 2)) // a: черга повна — втрачено
	<-a.tilesOut
	for len(b.tilesOut) > 0 {
		<-b.tilesOut
	}
	onAgentTiles(ns, tiles.Invalidate(2, 0))
	<-a.tilesOut
	<-b.tilesOut
	keep, _ := tiles.Keep(2, 0, 1920, 1080, []tiles.Rect{{X: 64, W: 64, H: 64}})
	a.tilesOut = make(chan []byte, 8)
	onAgentTiles(ns, keep)
	var ga, gb [][]byte
	for len(a.tilesOut) > 0 {
		ga = append(ga, <-a.tilesOut)
	}
	for len(b.tilesOut) > 0 {
		gb = append(gb, <-b.tilesOut)
	}
	if got := fmt.Sprint(summary(t, ga)); got != "[keep2:1 t2@64#2]" {
		t.Fatalf("lossy viewer %s", got)
	}
	if got := fmt.Sprint(summary(t, gb)); got != "[keep2:1]" {
		t.Fatalf("clean viewer %s", got)
	}
	if a.tilesLossy.Load() {
		t.Fatal("lossy flag not cleared")
	}
}

func TestEnqueueTilesBoundedInvalidateWins(t *testing.T) {
	vl := &viewerLeg{tilesOut: make(chan []byte, tilesQueueDepth)}
	for i := 0; i < tilesQueueDepth*3; i++ {
		enqueueTiles(vl, []byte{byte(i)}, false)
	}
	if len(vl.tilesOut) != tilesQueueDepth || vl.tilesDropped != uint64(2*tilesQueueDepth) {
		t.Fatalf("queue %d dropped %d", len(vl.tilesOut), vl.tilesDropped)
	}
	enqueueTiles(vl, []byte("inv"), true)
	var last []byte
	for len(vl.tilesOut) > 0 {
		last = <-vl.tilesOut
	}
	if string(last) != "inv" {
		t.Fatalf("invalidate lost, last=%q", last)
	}
}

func TestOnAgentTilesRejectsAndFansOut(t *testing.T) {
	ns := &nodeSession{nodeID: "tiles-unit"}
	a, b, c := addViewer(ns, newPC(t), newViewerTrack(t), "u1"), addViewer(ns, newPC(t), newViewerTrack(t), "u2"),
		addViewer(ns, newPC(t), newViewerTrack(t), "u3")
	t.Cleanup(func() { removeViewer(ns, a); removeViewer(ns, b); removeViewer(ns, c) })
	ns.mu.Lock()
	a.tilesOut, b.tilesOut = make(chan []byte, 4), make(chan []byte, 4) // c: no channel
	ns.mu.Unlock()

	onAgentTiles(ns, []byte("garbage"))
	onAgentTiles(ns, make([]byte, tiles.MaxMessage+1))
	bad := tileMsg(t, 1, 0, 10)
	bad[12] = 0xFF // x outside the source
	bad[13] = 0xFF
	onAgentTiles(ns, bad)
	if ns.tiles.dropped.Load() != 3 || len(a.tilesOut) != 0 {
		t.Fatalf("garbage forwarded: dropped=%d q=%d", ns.tiles.dropped.Load(), len(a.tilesOut))
	}
	good := tileMsg(t, 1, 64, 100)
	onAgentTiles(ns, good)
	good[40] = 0x55 // pion reuses buffers: hub must have copied
	if len(a.tilesOut) != 1 || len(b.tilesOut) != 1 {
		t.Fatal("not fanned out")
	}
	if got := <-a.tilesOut; got[40] != 0 {
		t.Fatal("message aliased the input buffer")
	}
	if len(ns.tiles.snapshot()) != 2 { // keep + тайл
		t.Fatal("not cached")
	}
}

// dialTilesAgent — агент з каналами oosc-ctl і oosc-tiles, як oo-agent з -text-tiles.
func dialTilesAgent(t *testing.T, node string) *webrtc.DataChannel {
	t.Helper()
	remote, err := agentAPI(t).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	if _, err := remote.CreateDataChannel("oosc-ctl", nil); err != nil {
		t.Fatal(err)
	}
	dc, err := remote.CreateDataChannel(tiles.ChannelLabel, nil)
	if err != nil {
		t.Fatal(err)
	}
	track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: h264FmtpLine,
	}, "video", "oo-screen")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.AddTrack(track); err != nil {
		t.Fatal(err)
	}
	exchange(t, remote, "agent", offerReq{Token: token, Node: node})
	return dc
}

// dialTilesViewer — браузер з config.textTiles: канал oosc-tiles до offer-а.
func dialTilesViewer(t *testing.T) (<-chan []byte, *webrtc.DataChannel) {
	t.Helper()
	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = remote.Close() })
	if _, err := remote.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	dc, err := remote.CreateDataChannel(tiles.ChannelLabel, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 64)
	dc.OnMessage(func(m webrtc.DataChannelMessage) { got <- append([]byte(nil), m.Data...) })
	exchange(t, remote, "viewer", offerReq{Token: token})
	return got, dc
}

func exchange(t *testing.T, pc *webrtc.PeerConnection, role string, req offerReq) {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	req.SDP = pc.LocalDescription().SDP
	body, _ := json.Marshal(req)
	w := httptest.NewRecorder()
	handleOffer(role)(w, httptest.NewRequest(http.MethodPost, "/offer/"+role, strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /offer/%s: %d %s", role, w.Code, w.Body.String())
	}
	var ans answerResp
	if err := json.Unmarshal(w.Body.Bytes(), &ans); err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans.SDP}); err != nil {
		t.Fatal(err)
	}
}

// TestTilesAgentToViewer — під прапорцем тайл агента доходить до глядача, а
// глядач, що прийшов пізніше, отримує кеш епізоду.
func TestTilesAgentToViewer(t *testing.T) {
	withTilesFlag(t, true)
	agentDC := dialTilesAgent(t, agentNodeIDEnv)
	var once sync.Once
	opened := make(chan struct{})
	agentDC.OnOpen(func() { once.Do(func() { close(opened) }) })
	if agentDC.ReadyState() == webrtc.DataChannelStateOpen {
		once.Do(func() { close(opened) })
	}
	select {
	case <-opened:
	case <-time.After(20 * time.Second):
		t.Fatal("agent tiles channel never opened")
	}

	got, vdc := dialTilesViewer(t)
	if !waitFor(20*time.Second, func() bool { return vdc.ReadyState() == webrtc.DataChannelStateOpen }) {
		t.Fatal("viewer tiles channel never opened")
	}
	ns := reg.get(agentNodeIDEnv)
	if !waitFor(5*time.Second, func() bool {
		ns.mu.Lock()
		defer ns.mu.Unlock()
		for _, vl := range ns.viewers {
			if vl.tilesOut != nil {
				return true
			}
		}
		return false
	}) {
		t.Fatal("hub never attached the viewer tiles queue")
	}

	msg := tileMsg(t, 5, 128, 500)
	if err := agentDC.Send(msg); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		d, err := tiles.Decode(m)
		if err != nil || d.Epoch != 5 || d.X != 128 {
			t.Fatalf("viewer got %v %+v", err, d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tile never reached the viewer")
	}

	// late viewer: replay from cache — порожній keep, далі повний набір
	got2, _ := dialTilesViewer(t)
	for _, want := range []uint8{tiles.TypeKeep, tiles.TypeTile} {
		select {
		case m := <-got2:
			if d, err := tiles.Decode(m); err != nil || d.Type != want || (want == tiles.TypeTile && d.X != 128) {
				t.Fatalf("replay %v %+v", err, d)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("late viewer got no replay")
		}
	}
}

// TestTilesFlagOff — без прапорця канал глядача не обслуговується.
func TestTilesFlagOff(t *testing.T) {
	withTilesFlag(t, false)
	_, vdc := dialTilesViewer(t)
	waitFor(3*time.Second, func() bool { return vdc.ReadyState() == webrtc.DataChannelStateOpen })
	for _, ns := range []*nodeSession{reg.get(agentNodeIDEnv)} {
		if ns == nil {
			continue
		}
		ns.mu.Lock()
		for _, vl := range ns.viewers {
			if vl.tilesOut != nil {
				t.Error("tiles queue attached with the flag off")
			}
		}
		ns.mu.Unlock()
	}
}

// TypeStill: розсилається, не кешується, у повній черзі витісняє тайл.
func TestOnAgentTilesStillNotCachedAndWins(t *testing.T) {
	ns := &nodeSession{nodeID: "tiles-still"}
	a := addViewer(ns, newPC(t), newViewerTrack(t), "u1")
	t.Cleanup(func() { removeViewer(ns, a) })
	ns.mu.Lock()
	a.tilesOut = make(chan []byte, 2)
	ns.mu.Unlock()

	onAgentTiles(ns, tileMsg(t, 5, 0, 10))
	onAgentTiles(ns, tileMsg(t, 5, 64, 10)) // черга повна
	onAgentTiles(ns, tiles.Still(5, 1))
	if len(ns.tiles.snapshot()) != 3 { // keep + 2 тайли
		t.Fatalf("still touched the cache: %d", len(ns.tiles.snapshot()))
	}
	<-a.tilesOut
	got := <-a.tilesOut
	if m, err := tiles.Decode(got); err != nil || m.Type != tiles.TypeStill {
		t.Fatalf("still lost in a full queue: %v %v", m, err)
	}
	if a.tilesDropped != 1 {
		t.Fatalf("dropped=%d", a.tilesDropped)
	}
}

// TestTilesDuplicateViewerChannels — одна viewer-нога з багатьма каналами
// 'oosc-tiles' отримує одну помпу; зайві канали закриваються хабом, а
// закриття каналу зупиняє помпу (горутини не течуть).
func TestTilesDuplicateViewerChannels(t *testing.T) {
	withTilesFlag(t, true)
	agentDC := dialTilesAgent(t, agentNodeIDEnv)
	if !waitFor(20*time.Second, func() bool { return agentDC.ReadyState() == webrtc.DataChannelStateOpen }) {
		t.Fatal("agent tiles channel never opened")
	}
	basePumps := tilesPumps.Load()
	baseG := runtime.NumGoroutine()

	remote, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if _, err := remote.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	const n = 40
	dcs := make([]*webrtc.DataChannel, n)
	for i := range dcs {
		if dcs[i], err = remote.CreateDataChannel(tiles.ChannelLabel, nil); err != nil {
			t.Fatal(err)
		}
	}
	exchange(t, remote, "viewer", offerReq{Token: token})

	// Хаб закриває всі, крім одного.
	if !waitFor(20*time.Second, func() bool {
		open, closed := 0, 0
		for _, dc := range dcs {
			switch dc.ReadyState() {
			case webrtc.DataChannelStateOpen:
				open++
			case webrtc.DataChannelStateClosed:
				closed++
			}
		}
		return open == 1 && closed == n-1
	}) {
		t.Fatal("duplicate tiles channels were not closed")
	}
	if p := tilesPumps.Load() - basePumps; p != 1 {
		t.Fatalf("pumps=%d, want 1", p)
	}
	if g := runtime.NumGoroutine() - baseG; g > 200 {
		t.Fatalf("goroutines grew by %d", g)
	}
	// Закриття єдиного живого каналу зупиняє помпу.
	for _, dc := range dcs {
		if dc.ReadyState() == webrtc.DataChannelStateOpen {
			_ = dc.Close()
		}
	}
	if !waitFor(10*time.Second, func() bool { return tilesPumps.Load() == basePumps }) {
		t.Fatalf("pump survived its channel: %d", tilesPumps.Load()-basePumps)
	}
}

// tilePNG — PNG signature + IHDR 64x64, padded with zeros to size bytes.
func tilePNG(size int) []byte {
	if size < 33 {
		size = 33
	}
	b := make([]byte, size)
	copy(b, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 13, 'I', 'H', 'D', 'R',
		0, 0, 0, 64, 0, 0, 0, 64, 8, 6, 0, 0, 0})
	return b
}
