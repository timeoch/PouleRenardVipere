package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"runtime/metrics"
	"testing"
	"time"
	"unsafe"
)

// fingerprint résume l'état complet d'une partie (positions, états) en un
// hash FNV déterministe.
func fingerprint(w *World) uint64 {
	const prime = 1099511628211
	h := uint64(14695981039346656037)
	mix := func(v uint64) {
		h ^= v
		h *= prime
	}
	for i := range w.Agents {
		a := &w.Agents[i]
		mix(math.Float64bits(a.X))
		mix(math.Float64bits(a.Y))
		mix(uint64(uint16(a.Stamina)) | uint64(uint16(a.Immune))<<16 | uint64(a.JailTicks)<<32 | uint64(a.Boost)<<48)
		flags := uint64(a.Species) | uint64(a.Role)<<2 | uint64(a.Jail)<<8
		if a.Prisoner {
			flags |= 1 << 4
		}
		if a.Exhausted {
			flags |= 1 << 5
		}
		if a.Resting {
			flags |= 1 << 6
		}
		mix(flags)
	}
	for _, it := range w.Items {
		if it.Active {
			mix(math.Float64bits(it.X) ^ uint64(it.Kind))
		}
	}
	mix(uint64(w.Tick))
	return h
}

func TestAgentFitsCacheLine(t *testing.T) {
	sz := unsafe.Sizeof(Agent{})
	n := DefaultPerSpecies * SpeciesCount
	t.Logf("sizeof(Agent) = %d octets, %d agents = %d Ko contigus", sz, n, int(sz)*n/1024)
	if naiveLayout {
		t.Skip("version naïve de la structure (tag naive_layout)")
	}
	if sz > 64 {
		t.Errorf("Agent doit tenir dans une ligne de cache de 64 octets, taille = %d", sz)
	}
}

func TestFoodChain(t *testing.T) {
	if PreyOf(Poule) != Vipere || PreyOf(Renard) != Poule || PreyOf(Vipere) != Renard {
		t.Fatal("chaîne alimentaire incorrecte")
	}
	for sp := Species(0); sp < SpeciesCount; sp++ {
		if PredatorOf(PreyOf(sp)) != sp || PreyOf(sp) == sp {
			t.Fatalf("PredatorOf n'est pas l'inverse de PreyOf pour %s", speciesNames[sp])
		}
	}
	w := NewWorld(MasterSeed)
	w.Reversed = true
	for sp := Species(0); sp < SpeciesCount; sp++ {
		if w.preyOf(sp) != PredatorOf(sp) || w.predOf(w.preyOf(sp)) != sp {
			t.Fatalf("chaîne inversée incorrecte pour %s", speciesNames[sp])
		}
	}
}

func TestParseRules(t *testing.T) {
	if r, err := ParseRules("toutes"); err != nil || r != AllRules() {
		t.Fatal("« toutes » doit activer toutes les règles")
	}
	if r, err := ParseRules("aucune"); err != nil || r != (Rules{}) {
		t.Fatal("« aucune » doit tout désactiver")
	}
	r, err := ParseRules("night, escape")
	if err != nil || !r.Night || !r.Escape || r.Terrain {
		t.Fatalf("liste mal lue : %+v, %v", r, err)
	}
	if r.String() != "night,escape" {
		t.Fatalf("String() = %q", r.String())
	}
	if _, err := ParseRules("night,volcan"); err == nil {
		t.Fatal("une règle inconnue doit être refusée")
	}
}

// TestDeterminism : la même graine produit exactement le même état, avec et
// sans les règles optionnelles.
func TestDeterminism(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  func(int64) Config
	}{{"toutes", DefaultConfig}, {"classique", ClassicConfig}} {
		run := func(seed int64) uint64 {
			w := NewWorldWith(c.cfg(seed))
			for i := 0; i < 300; i++ {
				w.Update()
			}
			return fingerprint(w)
		}
		a, b := run(MasterSeed), run(MasterSeed)
		if a != b {
			t.Fatalf("%s : simulation non déterministe : %x != %x", c.name, a, b)
		}
		if run(MasterSeed+1) == a {
			t.Fatalf("%s : deux graines différentes donnent le même état", c.name)
		}
		t.Logf("%s : empreinte après 300 ticks (seed=%d) : %x", c.name, MasterSeed, a)
	}
}

// TestParallelMatchesSequential : la phase parallèle donne exactement la même
// partie quel que soit le nombre de workers.
func TestParallelMatchesSequential(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  func(int64) Config
	}{{"toutes", DefaultConfig}, {"classique", ClassicConfig}} {
		run := func(workers int) uint64 {
			cfg := c.cfg(MasterSeed)
			cfg.Workers = workers
			w := NewWorldWith(cfg)
			for i := 0; i < 400; i++ {
				w.Update()
			}
			return fingerprint(w)
		}
		ref := run(1)
		for _, n := range []int{2, 3, 8, runtime.NumCPU()} {
			if got := run(n); got != ref {
				t.Fatalf("%s : %d workers donnent %x au lieu de %x (1 worker)", c.name, n, got, ref)
			}
		}
	}
}

// checkInvariants : prisonniers dans leur prison, agents libres jamais dans
// un mur ni dans l'eau, endurance bornée, statistiques cohérentes.
func checkInvariants(t *testing.T, w *World, perSpecies bool) {
	t.Helper()
	var free, jailed [SpeciesCount]int
	var held [SpeciesCount][SpeciesCount]int
	for i := range w.Agents {
		a := &w.Agents[i]
		if a.Stamina < 0 || a.Stamina > MaxStamina {
			t.Fatalf("tick %d : endurance hors bornes %d", w.Tick, a.Stamina)
		}
		if a.Prisoner {
			jailed[a.Species]++
			held[a.Jail][a.Species]++
			if a.Jail == a.Species || !w.Camps[a.Jail].Contains(a.X, a.Y) {
				t.Fatalf("tick %d : prisonnier %d hors de sa prison", w.Tick, i)
			}
			continue
		}
		free[a.Species]++
		if w.Blocked(a.X, a.Y) {
			t.Fatalf("tick %d : agent %d dans un obstacle (%.1f, %.1f)", w.Tick, i, a.X, a.Y)
		}
	}
	total := 0
	for sp := range free {
		total += free[sp] + jailed[sp]
		if perSpecies && free[sp]+jailed[sp] != w.PerSpecies {
			t.Fatalf("tick %d : %s non conservées (%d + %d)", w.Tick, speciesNames[sp], free[sp], jailed[sp])
		}
		if w.Stats[sp].Free != free[sp] || w.Stats[sp].Prisoners != jailed[sp] {
			t.Fatalf("tick %d : statistiques incohérentes pour %s", w.Tick, speciesNames[sp])
		}
	}
	if total != len(w.Agents) {
		t.Fatalf("tick %d : %d agents au lieu de %d", w.Tick, total, len(w.Agents))
	}
	if held != w.held {
		t.Fatalf("tick %d : décompte des captifs par camp incohérent", w.Tick)
	}
}

func TestInvariantsClassic(t *testing.T) {
	w := NewWorldWith(ClassicConfig(MasterSeed))
	for tick := 1; tick <= 1500; tick++ {
		w.Update()
		if tick%50 == 0 {
			checkInvariants(t, w, true)
		}
	}
}

// TestInvariantsAllRules : avec le syndrome de Stockholm, les espèces changent
// d'effectif mais le total est conservé.
func TestInvariantsAllRules(t *testing.T) {
	w := NewWorld(MasterSeed)
	for tick := 1; tick <= 4800; tick++ {
		w.Update()
		if tick%100 == 0 {
			checkInvariants(t, w, false)
		}
		if tick == w.ReversalTick() && !w.Reversed {
			t.Fatal("la chaîne aurait dû s'inverser")
		}
	}
	s := w.Stats
	t.Logf("tick %d : libres %d/%d/%d, évasions %d/%d/%d, recrues %d/%d/%d", w.Tick,
		s[0].Free, s[1].Free, s[2].Free, s[0].Escapes, s[1].Escapes, s[2].Escapes,
		s[0].Recruits, s[1].Recruits, s[2].Recruits)
}

// TestCustomSizeAndDuration : effectif et durée choisis au lancement, partie
// jouée jusqu'au bout (renversement à mi-manche, fin au plus tard au sifflet).
func TestCustomSizeAndDuration(t *testing.T) {
	cfg := DefaultConfig(MasterSeed)
	cfg.Rules.Stockholm = false
	cfg.PerSpecies = 500
	cfg.MaxTicks = MinutesToTicks(0.5)
	w := NewWorldWith(cfg)
	if len(w.Agents) != 3*500 || w.MaxTicks != 900 || w.ReversalTick() != 450 {
		t.Fatalf("configuration ignorée : %d agents, %d ticks", len(w.Agents), w.MaxTicks)
	}
	for !w.Finished {
		w.Update()
		if w.Tick == w.ReversalTick() && !w.Reversed {
			t.Fatal("la chaîne aurait dû s'inverser à mi-manche")
		}
		if w.Tick%100 == 0 {
			checkInvariants(t, w, true)
		}
	}
	if w.Tick > 900 {
		t.Fatalf("la manche aurait dû finir au tick 900, pas %d", w.Tick)
	}

	c := Config{PerSpecies: 1, MaxTicks: 10 * MaxMinutes * 60 * TargetTPS}.Clamp()
	if c.PerSpecies != MinPerSpecies || c.MaxTicks != MaxMinutes*60*TargetTPS {
		t.Fatalf("bornes non appliquées : %+v", c)
	}
	if c := (Config{}).Clamp(); c.PerSpecies != DefaultPerSpecies || c.MaxTicks != DefaultMinutes*60*TargetTPS {
		t.Fatalf("valeurs par défaut non appliquées : %+v", c)
	}
}

// TestSnapInterval : 5 s entre deux instantanés pour une partie normale ;
// plus pour les très grandes, afin de borner la mémoire du replay.
func TestSnapInterval(t *testing.T) {
	if got := snapInterval(3*DefaultPerSpecies, DefaultMinutes*60*TargetTPS); got != SnapEvery {
		t.Fatalf("partie par défaut : %d ticks entre deux instantanés, attendu %d", got, SnapEvery)
	}
	agents, ticks := 3*MaxPerSpecies, MaxMinutes*60*TargetTPS
	every := snapInterval(agents, ticks)
	mem := (ticks/every + 1) * (agents*(int(unsafe.Sizeof(Agent{}))+8) + 64<<10)
	if every%SnapEvery != 0 || mem > ReplayBudget*12/10 {
		t.Fatalf("plus grande partie : un instantané tous les %d ticks, %d Mo", every, mem>>20)
	}
	t.Logf("plus grande partie : un instantané toutes les %d s, %d Mo de replay", every/TargetTPS, mem>>20)
}

// TestReplayMatchesContinuous : revenir en arrière puis rejouer, entrées du
// joueur comprises, redonne exactement la même partie.
func TestReplayMatchesContinuous(t *testing.T) {
	s := newSession(DefaultConfig(MasterSeed), false)
	inputs := map[int][]Command{
		200: {{Cmd: "obstacle", X: 1200, Y: 2000, W: 300, H: 80}},
		250: {{Cmd: "control", I: 17}, {Cmd: "key", DX: 1, DY: -1, Sprint: true}},
		320: {{Cmd: "set", Key: "guards", Species: 1, Value: 0.2}, {Cmd: "key", DX: -1}},
		410: {{Cmd: "unobstacle", X: 1250, Y: 2030}, {Cmd: "set", Key: "vision", Value: 1.3}},
	}
	const end = 600
	var mid uint64
	for s.w.Tick < end {
		for _, c := range inputs[s.w.Tick] {
			s.input(c)
		}
		s.step()
		if s.w.Tick == 370 {
			mid = fingerprint(s.w)
		}
	}
	want := fingerprint(s.w)

	s.seek(370)
	if got := fingerprint(s.w); got != mid {
		t.Fatalf("retour au tick 370 : %x, attendu %x", got, mid)
	}
	for s.w.Tick < end {
		s.step()
	}
	if got := fingerprint(s.w); got != want {
		t.Fatalf("replay jusqu'au tick %d : %x, attendu %x", end, got, want)
	}

	// Une nouvelle entrée après un retour en arrière ouvre une autre branche.
	s.seek(300)
	s.input(Command{Cmd: "key", DX: 0, DY: 1})
	for s.w.Tick < end {
		s.step()
	}
	if fingerprint(s.w) == want {
		t.Fatal("la nouvelle branche devrait différer de l'originale")
	}
	if s.head != end {
		t.Fatalf("tête de la ligne de temps = %d, attendu %d", s.head, end)
	}
}

// TestFlowFieldsConnectCamps : avec la rivière, chaque camp reste accessible
// depuis les deux autres, et suivre le champ mène bien au camp.
func TestFlowFieldsConnectCamps(t *testing.T) {
	for seed := int64(1); seed <= 20; seed++ {
		w := NewWorld(seed)
		for c := range w.Camps {
			for o := range w.Camps {
				if c == o {
					continue
				}
				from := CellIndex(w.Camps[o].X, w.Camps[o].Y)
				if w.campFlow[c].Dist[from] == flowInf {
					t.Fatalf("seed %d : le camp %s est inaccessible depuis %s", seed, w.Camps[c].Name, w.Camps[o].Name)
				}
				// Descente du champ depuis l'autre camp.
				x, y := w.Camps[o].X, w.Camps[o].Y
				for step := 0; step < 4000 && !w.Camps[c].Contains(x, y); step++ {
					dx, dy := unit(w.flowDir(&w.campFlow[c], x, y, w.Camps[c].X, w.Camps[c].Y))
					if nx := x + dx*5; !w.Blocked(nx, y) {
						x = nx
					}
					if ny := y + dy*5; !w.Blocked(x, ny) {
						y = ny
					}
				}
				if !w.Camps[c].Contains(x, y) {
					t.Fatalf("seed %d : le chemin de %s vers %s n'aboutit pas (%.0f, %.0f)", seed, w.Camps[o].Name, w.Camps[c].Name, x, y)
				}
			}
		}
	}
}

func TestRolesFollowGenome(t *testing.T) {
	cfg := DefaultConfig(MasterSeed)
	cfg.Rules.DynamicRoles = false
	cfg.Genomes[Renard] = Genome{Guards: 0.2, Rescuers: 0.3, Caution: 1, Grit: 0.5}
	w := NewWorldWith(cfg)
	var n [SpeciesCount][4]int
	for i := range w.Agents {
		n[w.Agents[i].Species][w.Agents[i].Role]++
	}
	near := func(got int, want float64) bool { return math.Abs(float64(got)/float64(w.PerSpecies)-want) < 0.025 }
	if !near(n[Renard][Gardien], 0.2) || !near(n[Renard][Liberateur]+n[Renard][Leurre], 0.3) || !near(n[Renard][Leurre], 0.3*DecoyShare) {
		t.Fatalf("rôles des renards : %v", n[Renard])
	}
	if !near(n[Poule][Gardien], GuardRatio) || !near(n[Poule][Liberateur]+n[Poule][Leurre], RescuerRatio) {
		t.Fatalf("rôles des poules : %v", n[Poule])
	}
}

// TestNextRound : la manche suivante change de graine, repart de zéro et fait
// évoluer les stratégies à partir de celles de la manche finie.
func TestNextRound(t *testing.T) {
	s := newSession(DefaultConfig(MasterSeed), true)
	s.input(Command{Cmd: "set", Key: "grit", Species: 2, Value: 0.9})
	for i := 0; i < 200; i++ {
		s.step()
	}
	s.w.Winner = int(Vipere)
	played := s.w.Genomes
	s.nextRound()
	if s.gen != 1 || s.w.Seed != MasterSeed+1 || s.w.Tick != 0 || s.head != 0 || len(s.snaps) != 1 || len(s.inputs) != 0 {
		t.Fatalf("manche suivante mal initialisée : gen %d, seed %d, tick %d, %d instantanés", s.gen, s.w.Seed, s.w.Tick, len(s.snaps))
	}
	if s.w.Genomes != Evolve(played, int(Vipere), MasterSeed) {
		t.Fatal("les stratégies doivent évoluer à partir de celles de la manche finie")
	}
}

func TestEvolve(t *testing.T) {
	g := [SpeciesCount]Genome{DefaultGenome, {Guards: 0.3, Rescuers: 0.4, Caution: 1.6, Grit: 0.9}, DefaultGenome}
	a, b := Evolve(g, 1, 7), Evolve(g, 1, 7)
	if a != b {
		t.Fatal("l'évolution doit être déterministe")
	}
	if a[0].Guards <= g[0].Guards || a[2].Caution <= g[2].Caution {
		t.Fatalf("les perdants doivent se rapprocher du vainqueur : %+v", a)
	}
	for _, x := range a {
		if x != x.Clamp() {
			t.Fatalf("génome hors bornes : %+v", x)
		}
	}
}

func bruteNearest(w *World, sp Species, prisoners bool, x, y, maxDist float64, accept func(*Agent) bool) (int, float64) {
	best2 := maxDist * maxDist
	idx := -1
	for j := range w.Agents {
		b := &w.Agents[j]
		if b.Species != sp || b.Prisoner != prisoners {
			continue
		}
		dx, dy := b.X-x, b.Y-y
		d2 := dx*dx + dy*dy
		if d2 > best2 || (d2 == best2 && idx >= 0) || !accept(b) {
			continue
		}
		best2, idx = d2, j
	}
	if idx < 0 {
		return -1, 0
	}
	return idx, math.Sqrt(best2)
}

// TestGridNearestMatchesBruteForce : la recherche par anneaux renvoie
// exactement le même agent que le scan linéaire.
func TestGridNearestMatchesBruteForce(t *testing.T) {
	w := NewWorld(MasterSeed)
	for i := 0; i < 900; i++ {
		w.Update()
	}
	w.prepare()
	for sp := Species(0); sp < SpeciesCount; sp++ {
		w.jail[sp].Build(w.Agents, sp, true)
	}
	all := func(*Agent) bool { return true }
	rng := rand.New(rand.NewSource(7))
	checks := 0
	for q := 0; q < 1500; q++ {
		x := WallMargin + rng.Float64()*(WorldSize-2*WallMargin)
		y := WallMargin + rng.Float64()*(WorldSize-2*WallMargin)
		if q%5 == 0 { // requêtes ciblées dans les camps, là où sont les prisonniers
			c := &w.Camps[q%SpeciesCount]
			x, y = c.X+(rng.Float64()-0.5)*c.R, c.Y+(rng.Float64()-0.5)*c.R
		}
		for sp := Species(0); sp < SpeciesCount; sp++ {
			for _, r := range []float64{CaptureRadius, FleeRadius, VisionRadius, 1500, math.Inf(1)} {
				for _, f := range []func(*Agent) bool{all, w.vulnFn} {
					gi, gd := w.free[sp].Nearest(w.Agents, x, y, r, f)
					bi, bd := bruteNearest(w, sp, false, x, y, r, f)
					if gi != bi || gd != bd {
						t.Fatalf("libres %s r=%.0f (%.1f,%.1f) : grille=(%d,%.3f) brut=(%d,%.3f)", speciesNames[sp], r, x, y, gi, gd, bi, bd)
					}
				}
				gi, gd := w.jail[sp].Nearest(w.Agents, x, y, r, all)
				bi, bd := bruteNearest(w, sp, true, x, y, r, all)
				if gi != bi || gd != bd {
					t.Fatalf("prisonniers %s r=%.0f : grille=(%d,%.3f) brut=(%d,%.3f)", speciesNames[sp], r, gi, gd, bi, bd)
				}
				checks += 3
			}
		}
	}
	t.Logf("%d requêtes identiques au scan linéaire", checks)
}

// TestFullGameTimeline joue une partie complète (seed 42, toutes les règles)
// et vérifie qu'elle se termine et que le pire tick reste dans le budget.
func TestFullGameTimeline(t *testing.T) {
	if testing.Short() {
		t.Skip("partie complète, ignorée avec -short")
	}
	w := NewWorld(MasterSeed)
	const window = 600 // 20 s simulées
	worst := 0.0
	for !w.Finished {
		start := time.Now()
		k := 0
		for ; k < window && !w.Finished; k++ {
			w.Update()
		}
		ms := float64(time.Since(start).Microseconds()) / 1000.0 / float64(k)
		worst = max(worst, ms)
		s := w.Stats
		t.Logf("tick %5d (%4.0f s) %5.2f ms/tick | libres P/R/V %4d %4d %4d | prison %4d %4d %4d | captures %5d %5d %5d | libérations %4d %4d %4d | évasions %4d %4d %4d | recrues %4d %4d %4d",
			w.Tick, float64(w.Tick)/float64(TargetTPS), ms,
			s[0].Free, s[1].Free, s[2].Free, s[0].Prisoners, s[1].Prisoners, s[2].Prisoners,
			s[0].Captures, s[1].Captures, s[2].Captures, s[0].Rescues, s[1].Rescues, s[2].Rescues,
			s[0].Escapes, s[1].Escapes, s[2].Escapes, s[0].Recruits, s[1].Recruits, s[2].Recruits)
	}
	winner := "égalité"
	if w.Winner >= 0 {
		winner = speciesNames[w.Winner]
	}
	t.Logf("fin au tick %d (%.0f s) : victoire %s | pire fenêtre %.2f ms/tick (budget 33.3 ms)",
		w.Tick, float64(w.Tick)/float64(TargetTPS), winner, worst)
	if worst > 1000.0/float64(TargetTPS) {
		t.Errorf("le tick dépasse le budget de 30 TPS : %.2f ms", worst)
	}
}

// benchWorkers : workers de la phase parallèle pendant BenchmarkUpdate. 1 par
// défaut pour comparer les versions à charge égale (un seul cœur).
//
//	go test -run='^$' -bench=BenchmarkUpdate . -workers=8
var benchWorkers = flag.Int("workers", 1, "workers de la phase parallèle (BenchmarkUpdate)")

// warmed garde l'état de chaque partie de référence au tick de préchauffage.
// Il est calculé une fois par processus, sur tous les cœurs (même résultat
// qu'en séquentiel), puis restauré à chaque passe : -count=10 ne rejoue pas
// dix fois les 1 500 ticks de préchauffage, ce qui compte pour la version naïve.
var warmed = map[string]*Snapshot{}

func warmWorld(name string, cfg Config, ticks, workers int) *World {
	cfg.Workers = workers
	w := NewWorldWith(cfg)
	if ticks == 0 {
		return w
	}
	s, ok := warmed[name]
	if !ok {
		cfg.Workers = runtime.NumCPU()
		ref := NewWorldWith(cfg)
		for i := 0; i < ticks; i++ {
			ref.Update()
		}
		s = ref.Snapshot()
		warmed[name] = s
	}
	w.Restore(s)
	return w
}

var benchCases = []struct {
	name   string
	cfg    Config
	warmup int
}{
	{"Debut", DefaultConfig(MasterSeed), 0},
	{"MiPartie_t1500", DefaultConfig(MasterSeed), 1500},
	{"Classique_t1500", ClassicConfig(MasterSeed), 1500},
}

// gcSamples : cycles de ramasse-miettes et temps CPU estimé du GC (tous cœurs
// confondus), lus avant et après les ticks mesurés.
var gcSamples = []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/cpu/classes/gc/total:cpu-seconds"}}

func gcStats() (uint64, float64) {
	metrics.Read(gcSamples)
	return gcSamples[0].Value.Uint64(), gcSamples[1].Value.Float64()
}

func benchTicks(b *testing.B, w *World) {
	b.ReportAllocs()
	runtime.GC()
	b.ResetTimer()
	c0, g0 := gcStats()
	for i := 0; i < b.N; i++ {
		w.Update()
	}
	b.StopTimer()
	c1, g1 := gcStats()
	if sec := b.Elapsed().Seconds(); sec > 0 {
		b.ReportMetric(sec*1000/float64(b.N), "ms/tick")
		b.ReportMetric(float64(b.N)/sec, "tps")
		b.ReportMetric(float64(c1-c0)/float64(b.N), "gc/tick")
		b.ReportMetric((g1-g0)*1000/float64(b.N), "gc-ms/tick")
	}
}

// BenchmarkUpdate : protocole de mesure (seed 42, 100 ticks).
//
//	go test -run='^$' -bench=BenchmarkUpdate -benchtime=100x -benchmem -count=10 .
func BenchmarkUpdate(b *testing.B) {
	for _, c := range benchCases {
		b.Run(c.name, func(b *testing.B) {
			benchTicks(b, warmWorld(c.name, c.cfg, c.warmup, *benchWorkers))
		})
	}
}

// BenchmarkWorkers : passage à l'échelle de la phase parallèle en milieu de
// partie, de 1 worker à tous les cœurs logiques.
//
//	go test -run='^$' -bench=BenchmarkWorkers -benchtime=100x -count=10 .
func BenchmarkWorkers(b *testing.B) {
	c := benchCases[1]
	for _, n := range []int{1, 2, 3, 4, 6, 8, 10, 12, 16} {
		if n > runtime.GOMAXPROCS(0) {
			break
		}
		b.Run(fmt.Sprintf("w%02d", n), func(b *testing.B) {
			benchTicks(b, warmWorld(c.name, c.cfg, c.warmup, n))
		})
	}
}

// BenchmarkChunk : taille des lots de la phase parallèle. 2 250 = découpage
// fixe en 4 parts égales (un lot par worker).
//
//	go test -run='^$' -bench=BenchmarkChunk -benchtime=100x -count=10 .
func BenchmarkChunk(b *testing.B) {
	c := benchCases[1]
	defer func(old int64) { chunkAgents = old }(chunkAgents)
	for _, n := range []int{4, 8} {
		for _, size := range []int64{32, 64, 128, 256, 512, 1024, 2250} {
			b.Run(fmt.Sprintf("w%d/lot%04d", n, size), func(b *testing.B) {
				chunkAgents = size
				benchTicks(b, warmWorld(c.name, c.cfg, c.warmup, n))
			})
		}
	}
}

// BenchmarkSpawnPerAgent : la parallélisation naïve, une goroutine par agent
// et par phase, à comparer à BenchmarkWorkers.
func BenchmarkSpawnPerAgent(b *testing.B) {
	c := benchCases[1]
	w := warmWorld(c.name, c.cfg, c.warmup, runtime.GOMAXPROCS(0))
	w.spawnPerAgent = true
	benchTicks(b, w)
}

// BenchmarkPopulation : coût d'un tick selon l'effectif, pour voir quand le
// tableau des agents sort du cache L2 (3 × 3 000 agents = 576 Ko à 64 octets,
// 3 × 10 000 = 1,9 Mo). À comparer entre les tags naive_layout et défaut.
//
//	go test -run='^$' -bench=BenchmarkPopulation -benchtime=30x -count=10 .
func BenchmarkPopulation(b *testing.B) {
	for _, n := range []int{1000, 3000, 10000} {
		b.Run(fmt.Sprintf("n%05d", n), func(b *testing.B) {
			cfg := DefaultConfig(MasterSeed)
			cfg.PerSpecies = n
			benchTicks(b, warmWorld(fmt.Sprintf("pop%d", n), cfg, 300, *benchWorkers))
		})
	}
}
