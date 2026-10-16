// wsload ouvre plusieurs connexions WebSocket sur le serveur du jeu, comme
// autant d'onglets, et mesure ce qu'elles reçoivent : trames par seconde,
// taille des trames et débit total. Les trames binaires sont vérifiées
// (longueur de l'en-tête, nombre d'agents).
//
//	go run ./tools/wsload -clients 8 -duree 15s
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// msg : trame reçue avec son type (texte ou binaire), que le codec Message de
// x/net/websocket ne rend pas.
type msg struct {
	data   []byte
	binary bool
}

var rawCodec = websocket.Codec{Unmarshal: func(data []byte, payloadType byte, v any) error {
	m := v.(*msg)
	m.data, m.binary = data, payloadType == websocket.BinaryFrame
	return nil
}}

type stats struct {
	frames, bytes, bin, text int
	gaps                     []time.Duration // écart entre deux trames reçues
}

func main() {
	url := flag.String("url", "ws://localhost:8081/ws", "adresse WebSocket du serveur")
	clients := flag.Int("clients", 4, "connexions simultanées")
	duree := flag.Duration("duree", 15*time.Second, "durée de la mesure")
	warm := flag.Duration("chauffe", 3*time.Second, "durée ignorée au début (démarrage des parties)")
	flag.Parse()

	res := make([]stats, *clients)
	var wg sync.WaitGroup
	for k := 0; k < *clients; k++ {
		wg.Add(1)
		go func(st *stats) {
			defer wg.Done()
			ws, err := websocket.Dial(*url, "", "http://localhost/")
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer ws.Close()
			start := time.Now()
			var last time.Time
			var m msg
			for time.Since(start) < *warm+*duree {
				if err := rawCodec.Receive(ws, &m); err != nil {
					fmt.Fprintln(os.Stderr, err)
					return
				}
				now := time.Now()
				if now.Sub(start) < *warm {
					continue
				}
				if m.binary {
					n := int(binary.LittleEndian.Uint32(m.data))
					if n%2 != 0 || 4+n > len(m.data) || (len(m.data)-4-n)%6 != 0 {
						fmt.Fprintf(os.Stderr, "trame binaire invalide (%d octets, en-tête %d)\n", len(m.data), n)
						os.Exit(1)
					}
					st.bin++
				} else {
					if len(m.data) < 4096 {
						continue // message de contrôle (init, tournoi)
					}
					st.text++
				}
				st.frames++
				st.bytes += len(m.data)
				if !last.IsZero() {
					st.gaps = append(st.gaps, now.Sub(last))
				}
				last = now
			}
		}(&res[k])
	}
	wg.Wait()

	var tot stats
	for _, st := range res {
		tot.frames += st.frames
		tot.bytes += st.bytes
		tot.bin += st.bin
		tot.text += st.text
		tot.gaps = append(tot.gaps, st.gaps...)
	}
	if tot.frames == 0 {
		fmt.Println("aucune trame reçue")
		os.Exit(1)
	}
	sort.Slice(tot.gaps, func(i, j int) bool { return tot.gaps[i] < tot.gaps[j] })
	pct := func(p float64) time.Duration {
		if len(tot.gaps) == 0 {
			return 0
		}
		return tot.gaps[int(p*float64(len(tot.gaps)-1))]
	}
	sec := duree.Seconds()
	fmt.Printf("%d clients, %.0f s : %d trames (%d binaires, %d texte)\n", *clients, sec, tot.frames, tot.bin, tot.text)
	fmt.Printf("trames/s par client : %.1f\n", float64(tot.frames)/sec/float64(*clients))
	fmt.Printf("taille moyenne      : %.1f Ko\n", float64(tot.bytes)/float64(tot.frames)/1024)
	fmt.Printf("débit total         : %.2f Mo/s (%.2f Mo/s par client)\n",
		float64(tot.bytes)/sec/(1<<20), float64(tot.bytes)/sec/(1<<20)/float64(*clients))
	fmt.Printf("écart entre trames  : p50 %v, p99 %v, max %v\n",
		pct(0.50).Round(100*time.Microsecond), pct(0.99).Round(100*time.Microsecond), pct(1).Round(100*time.Microsecond))
}
