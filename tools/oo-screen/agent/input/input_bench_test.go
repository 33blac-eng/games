package input

import "testing"

// Ціна розбору й валідації однієї події вводу на агенті (кожен рух миші,
// до ~125/с від браузера після ліміту хаба).
func BenchmarkParseEvent(b *testing.B) {
	cases := map[string][]byte{
		"mouse_move": []byte(`{"v":1,"type":"mouse_move","x":0.5,"y":0.75}`),
		"key":        []byte(`{"v":1,"type":"key","scancode":30,"down":true}`),
	}
	for name, msg := range cases {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := ParseEvent(msg); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
