package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// GameResult résume une partie jouée sans interface.
type GameResult struct {
	Seed   int64             `json:"seed"`
	Winner int               `json:"winner"` // -1 : égalité
	Ticks  int               `json:"ticks"`
	Elim   bool              `json:"elim"` // finie par élimination avant le coup de sifflet
	Free   [SpeciesCount]int `json:"free"`

	Telemetry []TickRow `json:"-"` // état de chaque tick (PlayGame avec record)
}

// Tally agrège les résultats d'un tournoi.
type Tally struct {
	Total    int               `json:"total"`
	Done     int               `json:"done"`
	Wins     [SpeciesCount]int `json:"wins"`
	Draws    int               `json:"draws"`
	Elims    int               `json:"elims"` // parties finies par élimination
	AvgTicks float64           `json:"avg_ticks"`
}

func (t *Tally) Add(r GameResult) {
	t.AvgTicks = (t.AvgTicks*float64(t.Done) + float64(r.Ticks)) / float64(t.Done+1)
	t.Done++
	if r.Winner >= 0 {
		t.Wins[r.Winner]++
	} else {
		t.Draws++
	}
	if r.Elim {
		t.Elims++
	}
}

// PlayGame joue une partie complète ; false si ctx est annulé avant la fin.
// Avec record, GameResult.Telemetry reçoit l'état de chaque tick.
//
// Arrêt précoce : l'annulation de ctx est relayée dans un booléen atomique,
// lu à chaque tick pour le prix d'une lecture mémoire. Une partie abandonnée
// (tournoi arrêté, onglet fermé) s'arrête au tick suivant au lieu de finir sa
// tranche de 300 ticks.
func PlayGame(ctx context.Context, cfg Config, record bool) (GameResult, bool) {
	var stop atomic.Bool
	defer context.AfterFunc(ctx, func() { stop.Store(true) })()
	w := NewWorldWith(cfg)
	var rows []TickRow
	if record {
		rows = make([]TickRow, 0, SpeciesCount*w.MaxTicks)
	}
	for !w.Finished {
		if w.Tick%cancelEvery == 0 && stop.Load() {
			return GameResult{}, false
		}
		w.Update()
		if record {
			rows = appendTelemetry(rows, w)
		}
	}
	r := GameResult{Seed: cfg.Seed, Winner: w.Winner, Ticks: w.Tick, Elim: w.Tick < w.MaxTicks, Telemetry: rows}
	for sp := range w.Stats {
		r.Free[sp] = w.Stats[sp].Free
	}
	return r, true
}

// cancelEvery : intervalle de test de l'annulation, en ticks. 300 dans la
// première version ; gardé en variable pour mesurer l'écart (voir les tests).
var cancelEvery = 1

// TournamentWorkers : parties jouées en même temps (option -tournoi-workers).
// 0 : tous les cœurs logiques sauf un.
var TournamentWorkers = 0

func tournamentWorkers(n int) int {
	k := TournamentWorkers
	if k <= 0 {
		k = runtime.NumCPU() - 1
	}
	return min(max(1, k), n)
}

// RunTournament joue n parties (graines cfg.Seed … cfg.Seed+n-1) sur tous les
// cœurs sauf un, et appelle onResult après chacune depuis une seule goroutine.
// Chaque partie reste déterministe : seul l'ordre d'arrivée varie.
func RunTournament(ctx context.Context, cfg Config, n int, record bool, onResult func(GameResult)) {
	jobs := make(chan int64)
	results := make(chan GameResult)
	var wg sync.WaitGroup
	for k := 0; k < tournamentWorkers(n); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seed := range jobs {
				c := cfg
				c.Seed = seed
				c.Workers = 1 // le parallélisme est entre les parties, pas dedans
				r, ok := PlayGame(ctx, c, record)
				if !ok {
					return
				}
				select {
				case results <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for k := 0; k < n; k++ {
			select {
			case jobs <- cfg.Seed + int64(k):
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	for r := range results {
		onResult(r)
	}
}

// runTicksCLI joue une partie sans interface pendant n ticks (ou jusqu'à la
// fin) : c'est la commande chronométrée par hyperfine. Le bilan final
// permet de vérifier que deux versions jouent une partie comparable.
func runTicksCLI(cfg Config, n int) {
	w := NewWorldWith(cfg)
	start := time.Now()
	for w.Tick < n && !w.Finished {
		w.Update()
	}
	el := time.Since(start)
	fmt.Printf("%d ticks en %v (%.3f ms/tick, %d workers) | libres P/R/V %d %d %d | captures %d\n",
		w.Tick, el.Round(time.Millisecond), float64(el.Microseconds())/1000/float64(max(w.Tick, 1)), w.Workers,
		w.Stats[0].Free, w.Stats[1].Free, w.Stats[2].Free,
		w.Stats[0].Captures+w.Stats[1].Captures+w.Stats[2].Captures)
}

// clockText écrit une durée en ticks sous la forme m:ss.
func clockText(ticks int) string {
	s := ticks / TargetTPS
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func runTournamentCLI(cfg Config, n int, store *Store) {
	fmt.Printf("Tournoi : %d parties, graines %d à %d, 3 × %d agents, %s par manche, règles : %s, %d parties en parallèle\n",
		n, cfg.Seed, cfg.Seed+int64(n)-1, cfg.PerSpecies, clockText(cfg.MaxTicks), cfg.Rules, tournamentWorkers(n))
	start := time.Now()
	t := Tally{Total: n}
	ctx := context.Background()
	rows := 0
	var saving time.Duration
	RunTournament(ctx, cfg, n, store != nil, func(r GameResult) {
		t.Add(r)
		winner := "égalité"
		if r.Winner >= 0 {
			winner = speciesNames[r.Winner]
		}
		saved := ""
		if store != nil {
			t0 := time.Now()
			c := cfg
			c.Seed = r.Seed
			id, err := store.SaveRun(ctx, c, r, r.Telemetry)
			if err != nil {
				fmt.Fprintln(os.Stderr, "enregistrement :", err)
				os.Exit(1)
			}
			saving += time.Since(t0)
			rows += len(r.Telemetry)
			saved = fmt.Sprintf(" | partie n° %d, %d lignes en %v", id, len(r.Telemetry), time.Since(t0).Round(time.Millisecond))
		}
		fmt.Printf("[%3d/%d] graine %-6d %-8s en %5.1f s simulées | libres P/R/V %4d %4d %4d%s\n",
			t.Done, n, r.Seed, winner, float64(r.Ticks)/float64(TargetTPS), r.Free[0], r.Free[1], r.Free[2], saved)
	})
	el := time.Since(start)
	fmt.Printf("\nBilan en %.1f s de calcul (%.2f parties/s) :\n", el.Seconds(), float64(n)/el.Seconds())
	for sp := range t.Wins {
		fmt.Printf("  %-8s %3d victoires (%4.1f %%)\n", speciesNames[sp], t.Wins[sp], 100*float64(t.Wins[sp])/float64(n))
	}
	fmt.Printf("  Égalités %3d\n", t.Draws)
	fmt.Printf("  Durée moyenne %.0f s, %d parties finies par élimination\n", t.AvgTicks/float64(TargetTPS), t.Elims)
	if store != nil {
		fmt.Printf("  %d lignes de télémétrie enregistrées en %v (%.0f lignes/s)\n", rows, saving.Round(time.Millisecond), float64(rows)/saving.Seconds())
	}
}
