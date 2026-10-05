package codec444

import (
	"reflect"
	"testing"
)

func TestDisabledAlwaysH264(t *testing.T) {
	d := Negotiate(Config{}, Caps{Codecs: []string{"vp09.01.10.08.03", "av01.1.08M.08.0.000"}})
	if d.Codec != H264 {
		t.Fatalf("disabled must fall back to H264, got %v", d)
	}
}

func TestNegotiate(t *testing.T) {
	on := Config{Enabled: true}
	cases := []struct {
		caps []string
		cfg  Config
		want Codec
	}{
		{nil, on, H264},
		{[]string{"vp09.00.10.08", "avc1.4D0028"}, on, H264}, // profile 0 = 4:2:0
		{[]string{"av01.0.08M.08"}, on, H264},                // AV1 Main = 4:2:0
		{[]string{"VP09.01.10.08.03"}, on, VP9P1},
		{[]string{"vp09.01.10.08.03", "av01.1.08M.08.0.000"}, on, AV1P1},
		{[]string{"vp09.01.10.08.03", "av01.1.08M.08.0.000"}, Config{Enabled: true, Prefer: []Codec{VP9P1}}, VP9P1},
		{[]string{"av01.1.08M.08.0.000"}, Config{Enabled: true, Prefer: []Codec{VP9P1}}, H264},
	}
	for i, c := range cases {
		if got := Negotiate(c.cfg, Caps{Codecs: c.caps}).Codec; got != c.want {
			t.Errorf("case %d: got %s want %s", i, got, c.want)
		}
	}
}

func TestSDPHas444(t *testing.T) {
	sdp := "v=0\r\nm=audio 9 UDP 111\r\na=rtpmap:98 VP9/90000\r\na=fmtp:98 profile-id=1\r\n" +
		"m=video 9 UDP 96 98 100 45\r\na=rtpmap:96 VP9/90000\r\na=fmtp:96 profile-id=0\r\n" +
		"a=rtpmap:98 VP9/90000\r\na=fmtp:98 profile-id=1\r\na=rtpmap:45 AV1/90000\r\n" +
		"a=fmtp:45 level-idx=5;profile=1;tier=0\r\na=rtpmap:100 H264/90000\r\n"
	if got := SDPHas444(sdp); !reflect.DeepEqual(got, []Codec{AV1P1, VP9P1}) {
		t.Fatalf("got %v", got)
	}
	chrome := "m=video 9 UDP 96 98\r\na=rtpmap:96 VP9/90000\r\na=fmtp:96 profile-id=0\r\na=rtpmap:98 AV1/90000\r\n"
	if got := SDPHas444(chrome); len(got) != 0 {
		t.Fatalf("profile 0 / AV1 without profile=1 must not count as 4:4:4: %v", got)
	}
}

func TestFFmpegArgs(t *testing.T) {
	if FFmpegArgs(H264) != nil {
		t.Fatal("H264 uses agent encoder")
	}
	for _, c := range []Codec{VP9P1, AV1P1} {
		a := FFmpegArgs(c)
		if len(a) < 4 || a[3] != "1" {
			t.Fatalf("%s: %v", c, a)
		}
	}
}
