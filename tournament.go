package main

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// GameResult résume une partie jouée sans interface.
type GameResult struct {
	Seed   int64             `json:"seed"`
	Winner int               `json:"winner"` // -1 : égalité
	Ticks  int               `json:"ticks"`
	Elim   bool              `json:"elim"` // finie par élimination avant le coup de sifflet
	Free   [SpeciesCount]int `json:"free"`
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
func PlayGame(ctx context.Context, cfg Config) (GameResult, bool) {
	w := NewWorldWith(cfg)
	for !w.Finished {
		if w.Tick%300 == 0 && ctx.Err() != nil {
			return GameResult{}, false
		}
		w.Update()
	}
	r := GameResult{Seed: cfg.Seed, Winner: w.Winner, Ticks: w.Tick, Elim: w.Tick < w.MaxTicks}
	for sp := range w.Stats {
		r.Free[sp] = w.Stats[sp].Free
	}
	return r, true
}

// RunTournament joue n parties (graines cfg.Seed … cfg.Seed+n-1) sur tous les
// cœurs sauf un, et appelle onResult après chacune depuis une seule goroutine.
// Chaque partie reste déterministe : seul l'ordre d'arrivée varie.
func RunTournament(ctx context.Context, cfg Config, n int, onResult func(GameResult)) {
	jobs := make(chan int64)
	results := make(chan GameResult)
	var wg sync.WaitGroup
	for k := 0; k < min(max(1, runtime.NumCPU()-1), n); k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seed := range jobs {
				c := cfg
				c.Seed = seed
				r, ok := PlayGame(ctx, c)
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

// clockText écrit une durée en ticks sous la forme m:ss.
func clockText(ticks int) string {
	s := ticks / TargetTPS
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func runTournamentCLI(cfg Config, n int) {
	fmt.Printf("Tournoi : %d parties, graines %d à %d, 3 × %d agents, %s par manche, règles : %s, %d cœurs\n",
		n, cfg.Seed, cfg.Seed+int64(n)-1, cfg.PerSpecies, clockText(cfg.MaxTicks), cfg.Rules, min(max(1, runtime.NumCPU()-1), n))
	start := time.Now()
	t := Tally{Total: n}
	RunTournament(context.Background(), cfg, n, func(r GameResult) {
		t.Add(r)
		winner := "égalité"
		if r.Winner >= 0 {
			winner = speciesNames[r.Winner]
		}
		fmt.Printf("[%3d/%d] graine %-6d %-8s en %5.1f s simulées | libres P/R/V %4d %4d %4d\n",
			t.Done, n, r.Seed, winner, float64(r.Ticks)/float64(TargetTPS), r.Free[0], r.Free[1], r.Free[2])
	})
	fmt.Printf("\nBilan en %.0f s de calcul :\n", time.Since(start).Seconds())
	for sp := range t.Wins {
		fmt.Printf("  %-8s %3d victoires (%4.1f %%)\n", speciesNames[sp], t.Wins[sp], 100*float64(t.Wins[sp])/float64(n))
	}
	fmt.Printf("  Égalités %3d\n", t.Draws)
	fmt.Printf("  Durée moyenne %.0f s, %d parties finies par élimination\n", t.AvgTicks/float64(TargetTPS), t.Elims)
}
