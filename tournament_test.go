package main

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"
)

// shortGames : parties de 10 s simulées (300 ticks) pour mesurer le débit
// d'un tournoi sans attendre 5 minutes de jeu par partie.
func shortGames() Config {
	cfg := DefaultConfig(MasterSeed)
	cfg.MaxTicks = 10 * TargetTPS
	return cfg.Clamp()
}

// BenchmarkTournament : débit d'un tournoi (parties/s) selon le nombre de
// parties jouées en même temps. Chaque passe joue 4 parties courtes par
// worker : tous les workers restent occupés jusqu'au dernier tour (avec un
// nombre fixe de parties, 24 parties sur 11 workers laissent 9 cœurs
// inactifs au troisième tour et faussent la comparaison).
//
//	go test -run='^$' -bench=BenchmarkTournament -benchtime=1x -count=5 .
func BenchmarkTournament(b *testing.B) {
	cfg := shortGames()
	defer func(old int) { TournamentWorkers = old }(TournamentWorkers)
	for _, k := range []int{1, 2, 4, 6, 8, 11} {
		if k > runtime.NumCPU() {
			break
		}
		games := 4 * k
		b.Run(fmt.Sprintf("p%02d", k), func(b *testing.B) {
			TournamentWorkers = k
			for i := 0; i < b.N; i++ {
				done := 0
				RunTournament(context.Background(), cfg, games, func(GameResult) { done++ })
				if done != games {
					b.Fatalf("%d parties sur %d", done, games)
				}
			}
			b.ReportMetric(float64(b.N*games)/b.Elapsed().Seconds(), "parties/s")
		})
	}
}

// TestTournamentCancelLatency : temps entre l'arrêt d'un tournoi et le retour
// de RunTournament, en testant l'annulation à chaque tick ou tous les 300
// ticks comme dans la première version.
//
//	go test -run TestTournamentCancelLatency -v .
func TestTournamentCancelLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("mesure, ignorée avec -short")
	}
	defer func(old int) { cancelEvery = old }(cancelEvery)
	cfg := DefaultConfig(MasterSeed)
	for _, every := range []int{300, 1} {
		cancelEvery = every
		var worst, sum time.Duration
		const runs = 5
		for r := 0; r < runs; r++ {
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan time.Time)
			go func() {
				RunTournament(ctx, cfg, 50, func(GameResult) {})
				done <- time.Now()
			}()
			time.Sleep(time.Duration(1500+300*r) * time.Millisecond) // en pleine partie
			t0 := time.Now()
			cancel()
			lat := (<-done).Sub(t0)
			sum += lat
			worst = max(worst, lat)
		}
		t.Logf("annulation testée tous les %3d ticks : latence moyenne %v, pire %v (%d essais)",
			every, (sum / runs).Round(100*time.Microsecond), worst.Round(100*time.Microsecond), runs)
	}
}
