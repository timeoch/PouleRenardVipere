// hfstats lit un ou plusieurs exports JSON de hyperfine (--export-json) et
// écrit un tableau Markdown : moyenne, médiane, écart-type, variance,
// coefficient de variation, min, max et accélération par rapport à la
// première commande. hyperfine donne l'écart-type mais pas la variance.
//
//	go run ./tools/hfstats bench/hyperfine.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
)

type result struct {
	Command string    `json:"command"`
	Mean    float64   `json:"mean"`
	Stddev  float64   `json:"stddev"`
	Median  float64   `json:"median"`
	Min     float64   `json:"min"`
	Max     float64   `json:"max"`
	Times   []float64 `json:"times"`
}

func main() {
	flag.Parse()
	var all []result
	for _, path := range flag.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var doc struct {
			Results []result `json:"results"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			fmt.Fprintln(os.Stderr, path, err)
			os.Exit(1)
		}
		all = append(all, doc.Results...)
	}
	if len(all) == 0 {
		fmt.Fprintln(os.Stderr, "usage : hfstats fichier.json…")
		os.Exit(2)
	}
	fmt.Println("| Commande | n | Moyenne (s) | Médiane (s) | Écart-type (s) | Variance (s²) | CV | Min (s) | Max (s) | Accélération |")
	fmt.Println("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	ref := all[0].Mean
	for _, r := range all {
		n := len(r.Times)
		// Variance d'échantillon (n-1), recalculée sur les temps bruts : elle
		// doit redonner l'écart-type affiché par hyperfine.
		var sum, sq float64
		for _, t := range r.Times {
			sum += t
		}
		mean := sum / float64(n)
		for _, t := range r.Times {
			sq += (t - mean) * (t - mean)
		}
		variance := 0.0
		if n > 1 {
			variance = sq / float64(n-1)
		}
		sorted := append([]float64(nil), r.Times...)
		sort.Float64s(sorted)
		median := sorted[n/2]
		if n%2 == 0 {
			median = (sorted[n/2-1] + sorted[n/2]) / 2
		}
		fmt.Printf("| `%s` | %d | %.4f | %.4f | %.4f | %.3g | %.1f %% | %.4f | %.4f | ×%.2f |\n",
			r.Command, n, mean, median, math.Sqrt(variance), variance, 100*math.Sqrt(variance)/mean,
			sorted[0], sorted[n-1], ref/mean)
	}
}
