package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		SrcW: 1920, SrcH: 1080, Format: tiles.FormatPNG, Payload: make([]byte, size)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTilesCacheEpochs(t *testing.T) {
	var c tilesCache
	c.remember(&tiles.Msg{Type: tiles.TypeTile, Epoch: 1}, []byte("a"))
	c.remember(&tiles.Msg{Type: tiles.TypeTile, Epoch: 1}, []byte("b"))
	if n := len(c.snapshot()); n != 2 {
		t.Fatalf("cache %d", n)
	}
	c.remember(&tiles.Msg{Type: tiles.TypeInvalidate, Epoch: 2}, []byte("inv"))
	if s := c.snapshot(); len(s) != 1 || string(s[0]) != "inv" {
		t.Fatalf("after invalidate %q", s)
	}
	c.remember(&tiles.Msg{Type: tiles.TypeTile, Epoch: 2}, []byte("c"))
	c.remember(&tiles.Msg{Type: tiles.TypeTile, Epoch: 3}, []byte("d")) // new epoch w/o invalidate
	if s := c.snapshot(); len(s) != 1 || string(s[0]) != "d" {
		t.Fatalf("epoch switch %q", s)
	}
	big := make([]byte, tilesCacheBytes)
	c.remember(&tiles.Msg{Type: tiles.TypeTile, Epoch: 3}, big)
	if len(c.snapshot()) != 1 {
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
	if len(ns.tiles.snapshot()) != 1 {
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

	// late viewer: replay from cache
	got2, _ := dialTilesViewer(t)
	select {
	case m := <-got2:
		if d, err := tiles.Decode(m); err != nil || d.X != 128 {
			t.Fatalf("replay %v %+v", err, d)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("late viewer got no replay")
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
