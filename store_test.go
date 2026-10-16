package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Les tests et benchmarks de ce fichier demandent une base PostgreSQL :
//
//	docker compose up -d --wait
//	PRV_DB=postgres://prv:prv@localhost:5434/prv go test -run TestStore .
//
// Les mesures de requêtes demandent en plus des données : un tournoi de 40
// parties (go run . -tournoi 40 -db $PRV_DB) donne 1,08 million de lignes.

func testStore(tb testing.TB) *Store {
	url := os.Getenv("PRV_DB")
	if url == "" {
		tb.Skip("PRV_DB non défini : pas de base PostgreSQL")
	}
	s, err := OpenStore(context.Background(), url)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(s.Close)
	return s
}

// curveRun : une partie complète enregistrée (la plus ancienne), sinon skip.
func curveRun(tb testing.TB, s *Store, minRows int) int64 {
	ctx := context.Background()
	var total int64
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM run_ticks`).Scan(&total); err != nil {
		tb.Fatal(err)
	}
	if total < int64(minRows) {
		tb.Skipf("%d lignes en base, il en faut %d (go run . -tournoi 40 -db $PRV_DB)", total, minRows)
	}
	var run int64
	err := s.pool.QueryRow(ctx, `SELECT id FROM runs WHERE ticks >= 9000 ORDER BY id LIMIT 1`).Scan(&run)
	if err != nil {
		tb.Skip("aucune partie complète en base")
	}
	return run
}

// telemetrySample : télémétrie réelle d'une partie de 30 s (2 700 lignes).
func telemetrySample(tb testing.TB) []TickRow {
	cfg := DefaultConfig(MasterSeed)
	cfg.MaxTicks = 30 * TargetTPS
	r, ok := PlayGame(context.Background(), cfg, true)
	if !ok || len(r.Telemetry) != SpeciesCount*cfg.MaxTicks {
		tb.Fatalf("télémétrie incomplète : %d lignes", len(r.Telemetry))
	}
	return r.Telemetry
}

func TestStoreRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rows := telemetrySample(t)
	cfg := DefaultConfig(MasterSeed)
	id, err := s.SaveRun(ctx, cfg, GameResult{Seed: MasterSeed, Winner: -1, Ticks: 900}, rows)
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Exec(ctx, `DELETE FROM run_ticks WHERE run_id = $1; DELETE FROM runs WHERE id = $1`, id)
	got, err := s.Curve(ctx, id, 100, 199)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 300 {
		t.Fatalf("%d lignes lues au lieu de 300", len(got))
	}
	for _, r := range got {
		want := rows[(int(r.Tick)-1)*SpeciesCount+int(r.Species)]
		if r != want {
			t.Fatalf("tick %d espèce %d : %+v au lieu de %+v", r.Tick, r.Species, r, want)
		}
	}
	if _, ok := s.cache.get(curveKey{id, 100, 199}); !ok {
		t.Fatal("la courbe lue n'est pas dans le cache")
	}
}

// BenchmarkSaveTelemetry : 2 700 lignes (une partie de 30 s) écrites ligne à
// ligne (un INSERT, donc un aller-retour, par ligne) ou en un seul COPY.
func BenchmarkSaveTelemetry(b *testing.B) {
	s := testStore(b)
	ctx := context.Background()
	rows := telemetrySample(b)
	const scratch = -1 // lignes de mesure, supprimées à la fin
	cleanup := func() { s.pool.Exec(ctx, `DELETE FROM run_ticks WHERE run_id = $1`, scratch) }
	cleanup()
	b.Cleanup(cleanup)

	b.Run("insert_unitaire", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			for k := range rows {
				t := &rows[k]
				if _, err := s.pool.Exec(ctx,
					`INSERT INTO run_ticks (run_id, tick, species, free, prisoners, exhausted) VALUES ($1, $2, $3, $4, $5, $6)`,
					scratch, t.Tick, t.Species, t.Free, t.Prisoners, t.Exhausted); err != nil {
					b.Fatal(err)
				}
			}
		}
		b.ReportMetric(float64(b.N*len(rows))/b.Elapsed().Seconds(), "lignes/s")
	})
	cleanup()
	b.Run("copy", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := s.SaveRun(ctx, DefaultConfig(MasterSeed), GameResult{Seed: scratch, Winner: -1}, rows); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		s.pool.Exec(ctx, `DELETE FROM run_ticks WHERE run_id IN (SELECT id FROM runs WHERE seed = $1); DELETE FROM runs WHERE seed = $1`, scratch)
		b.ReportMetric(float64(b.N*len(rows))/b.Elapsed().Seconds(), "lignes/s")
	})
}

// Fenêtre de la courbe mesurée : 10 s en milieu de partie (900 lignes).
const curveFrom, curveTo = 4500, 4799

// BenchmarkCurve : la même requête sans index (parcours séquentiel de la
// table), avec l'index composite (run_id, tick), puis servie par le cache LRU.
func BenchmarkCurve(b *testing.B) {
	s := testStore(b)
	ctx := context.Background()
	run := curveRun(b, s, 1_000_000)
	query := func(b *testing.B) {
		n := 0
		for i := 0; i < b.N; i++ {
			rows, err := s.curveDB(ctx, run, curveFrom, curveTo)
			if err != nil {
				b.Fatal(err)
			}
			n = len(rows)
		}
		if n != 3*(curveTo-curveFrom+1) {
			b.Fatalf("%d lignes au lieu de %d", n, 3*(curveTo-curveFrom+1))
		}
	}
	b.Run("sans_index", func(b *testing.B) {
		mustExec(b, s, dropIndexSQL, "ANALYZE run_ticks")
		b.ResetTimer()
		query(b)
	})
	b.Run("index", func(b *testing.B) {
		mustExec(b, s, indexSQL, "ANALYZE run_ticks")
		b.ResetTimer()
		query(b)
	})
	b.Run("cache_lru", func(b *testing.B) {
		if _, err := s.Curve(ctx, run, curveFrom, curveTo); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := s.Curve(ctx, run, curveFrom, curveTo); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func mustExec(tb testing.TB, s *Store, sqls ...string) {
	for _, q := range sqls {
		if _, err := s.pool.Exec(context.Background(), q); err != nil {
			tb.Fatal(err)
		}
	}
}

// TestExplainCurve affiche le plan de la requête de courbe sans puis avec
// l'index (EXPLAIN ANALYZE, BUFFERS). L'index reste en place à la fin.
//
//	PRV_DB=... go test -run TestExplainCurve -v .
func TestExplainCurve(t *testing.T) {
	s := testStore(t)
	run := curveRun(t, s, 1_000_000)
	explain := func(label string) {
		r, err := s.pool.Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS) "+curveSQL, run, curveFrom, curveTo)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for r.Next() {
			var l string
			if err := r.Scan(&l); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, l)
		}
		r.Close()
		t.Logf("%s (partie %d, ticks %d à %d) :\n%s", label, run, curveFrom, curveTo, strings.Join(lines, "\n"))
	}
	mustExec(t, s, dropIndexSQL, "ANALYZE run_ticks")
	explain("sans index")
	explain("sans index, 2e passe (pages en cache)")
	mustExec(t, s, indexSQL, "ANALYZE run_ticks")
	explain("avec l'index (run_id, tick)")
	explain("avec l'index, 2e passe")
}
