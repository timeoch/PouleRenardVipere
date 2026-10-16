package main

import (
	"encoding/binary"
	"encoding/json"
	"testing"
)

// frameSession : session en milieu de partie (t = 1500), état partagé avec
// les benchmarks de la simulation.
func frameSession() *session {
	s := &session{cfg: DefaultConfig(MasterSeed), speed: 1}
	c := benchCases[1]
	s.w = warmWorld(c.name, c.cfg, c.warmup, 1)
	s.head = s.w.Tick
	return s
}

// TestBinaryFrameRoundTrip : l'en-tête binaire relu en JSON et les agents
// relus en int16 redonnent exactement la trame d'origine.
func TestBinaryFrameRoundTrip(t *testing.T) {
	s := frameSession()
	f := s.frame(nil, nil, 30, 2.5)
	data, err := newFrameEncoder().appendBinary(nil, &f)
	if err != nil {
		t.Fatal(err)
	}
	n := int(binary.LittleEndian.Uint32(data))
	if n%2 != 0 {
		t.Fatalf("en-tête de longueur impaire : %d", n)
	}
	var hdr frameMessage
	if err := json.Unmarshal(data[4:4+n], &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr.Tick != f.Tick || hdr.Stats != f.Stats || len(hdr.Agents) != 0 {
		t.Fatalf("en-tête incorrect : tick %d, %d agents dans l'en-tête", hdr.Tick, len(hdr.Agents))
	}
	body := data[4+n:]
	if len(body) != 2*len(f.Agents) {
		t.Fatalf("%d octets d'agents au lieu de %d", len(body), 2*len(f.Agents))
	}
	for k, v := range f.Agents {
		if got := int16(binary.LittleEndian.Uint16(body[2*k:])); got != v {
			t.Fatalf("agent %d, champ %d : %d au lieu de %d", k/3, k%3, got, v)
		}
	}
	js, _ := json.Marshal(f)
	t.Logf("trame t=%d : JSON %d octets, binaire %d octets (en-tête %d) : %.1f× moins",
		f.Tick, len(js), len(data), n, float64(len(js))/float64(len(data)))
}

var frameSink int

// BenchmarkFrame : coût de production d'une trame (9 000 agents, t = 1500).
// json : format d'origine, json.Marshal puis conversion en string à l'envoi.
// binary : en-tête JSON réutilisé, agents copiés en bloc, tampon recyclé.
//
//	go test -run='^$' -bench=BenchmarkFrame -benchmem -count=10 .
func BenchmarkFrame(b *testing.B) {
	s := frameSession()
	b.Run("json", func(b *testing.B) {
		var buf []int16
		var ev []Event
		size := 0
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			f := s.frame(buf[:0], ev[:0], 30, 2.5)
			buf, ev = f.Agents, f.Events
			data, err := json.Marshal(f)
			if err != nil {
				b.Fatal(err)
			}
			str := string(data) // websocket.Message.Send(ws, string(data))
			size = len(str)
			frameSink += int(str[len(str)-1])
		}
		b.ReportMetric(float64(size), "B/frame")
	})
	b.Run("binary", func(b *testing.B) {
		var buf []int16
		var ev []Event
		fe := newFrameEncoder()
		size := 0
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			f := s.frame(buf[:0], ev[:0], 30, 2.5)
			buf, ev = f.Agents, f.Events
			bp := framePool.Get().(*[]byte)
			data, err := fe.appendBinary((*bp)[:0], &f)
			if err != nil {
				b.Fatal(err)
			}
			*bp = data
			size = len(data)
			frameSink += int(data[len(data)-1])
			framePool.Put(bp)
		}
		b.ReportMetric(float64(size), "B/frame")
	})
}
