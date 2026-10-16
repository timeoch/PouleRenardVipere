package main

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// Phase parallèle : décision et déplacement des agents sur un pool borné
// ---------------------------------------------------------------------------

// chunkAgents : taille d'un lot. Les agents ne coûtent pas tous pareil (un
// prisonnier ne fait rien, un chasseur teste des lignes de vue) : des petits
// lots pris à la demande équilibrent mieux la charge qu'un découpage en N
// parts égales, surtout avec des cœurs performants et des cœurs économes.
var chunkAgents int64 = 128

// DefaultWorkers : nombre de workers d'une partie quand la configuration n'en
// fixe pas (option -workers). 0 : la moitié des cœurs logiques, soit les cœurs
// physiques d'une machine avec hyper-threading. Sur l'i5-12450H (4 cœurs P
// avec HT + 4 cœurs E), le gain plafonne dès 4 workers (BenchmarkWorkers) :
// au-delà, on brûle du CPU sans accélérer le tick.
var DefaultWorkers = 0

const (
	phaseDecide  uint8 = iota // vitesse de chaque agent
	phaseMove                 // déplacement
	phaseCapture              // proie au contact de chaque prédateur
)

// agentRand : générateur splitmix64 tiré de (graine, tick, agent). Chaque
// agent a sa propre suite à chaque tick : la partie ne dépend ni de l'ordre
// de traitement ni du nombre de workers.
type agentRand struct{ s uint64 }

func (r *agentRand) seed(seed int64, tick, i int) {
	r.s = uint64(seed)*0x9E3779B97F4A7C15 ^ uint64(tick)*0xD1B54A32D192ED03 ^ uint64(i+1)*0x94D049BB133111EB
}

func (r *agentRand) Float64() float64 {
	r.s += 0x9E3779B97F4A7C15
	z := r.s
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z ^= z >> 31
	return float64(z>>11) / (1 << 53)
}

// mind : contexte de décision d'un worker. Tout ce que la décision écrit en
// dehors de l'agent traité est ici (générateur, position de la requête pour
// les hautes herbes, agent qui cherche sa meute). Le remplissage final isole
// chaque contexte sur ses propres lignes de cache : sans lui, deux workers
// écriraient sur la même ligne à chaque tirage (faux partage).
type mind struct {
	w      *World
	rng    agentRand
	qx, qy float64
	self   *Agent

	seeFn   func(*Agent) bool
	guardFn func(*Agent) bool
	packFn  func(*Agent) bool

	_ [64]byte
}

func (m *mind) init(w *World) {
	m.w = w
	m.seeFn = m.canSee
	m.guardFn = m.isGuardTarget
	m.packFn = func(b *Agent) bool { return b != m.self && !b.Exhausted }
}

// batch : une phase d'un tick, partagée entre la goroutine de la partie et
// les workers du pool. Préalloué dans World : aucune allocation par tick.
type batch struct {
	w     *World
	phase uint8
	next  atomic.Int64 // premier agent du prochain lot
	slot  atomic.Int32 // contexte de décision du prochain participant
	wg    sync.WaitGroup
}

func (b *batch) work() {
	w := b.w
	m := &w.minds[b.slot.Add(1)-1]
	n := int64(len(w.Agents))
	for {
		lo := b.next.Add(chunkAgents) - chunkAgents
		if lo >= n {
			return
		}
		hi := min(lo+chunkAgents, n)
		switch b.phase {
		case phaseDecide:
			for i := lo; i < hi; i++ {
				m.decide(int(i))
			}
		case phaseMove:
			for i := lo; i < hi; i++ {
				w.move(int(i))
			}
		case phaseCapture:
			for i := lo; i < hi; i++ {
				w.capTarget[i] = int32(w.captureTarget(int(i)))
			}
		}
	}
}

// pool : workers partagés par toutes les parties du processus. Leur nombre
// est borné une fois pour toutes à GOMAXPROCS - 1 (la goroutine de la partie
// travaille aussi) : dix onglets ouverts ne lancent pas dix fois plus de
// threads, ils se partagent les mêmes cœurs.
var pool struct {
	once sync.Once
	jobs chan *batch
}

func poolJobs() chan *batch {
	pool.once.Do(func() {
		n := runtime.GOMAXPROCS(0) - 1
		pool.jobs = make(chan *batch, 4*max(n, 1))
		for k := 0; k < n; k++ {
			go func() {
				for b := range pool.jobs {
					b.work()
					b.wg.Done()
				}
			}()
		}
	})
	return pool.jobs
}

func (w *World) initWorkers(n int) {
	if n <= 0 {
		n = DefaultWorkers
	}
	if n <= 0 {
		n = max(1, runtime.NumCPU()/2)
	}
	w.Workers = min(n, runtime.GOMAXPROCS(0))
	w.minds = make([]mind, w.Workers)
	for k := range w.minds {
		w.minds[k].init(w)
	}
}

// parallel exécute une phase sur w.Workers goroutines (celle de la partie et
// w.Workers-1 workers du pool), puis attend la fin de tous les lots.
func (w *World) parallel(phase uint8) {
	b := &w.batch
	b.w, b.phase = w, phase
	b.next.Store(0)
	b.slot.Store(0)
	helpers := w.Workers - 1
	if helpers <= 0 {
		b.work()
		return
	}
	jobs := poolJobs()
	b.wg.Add(helpers)
	for k := 0; k < helpers; k++ {
		jobs <- b
	}
	b.work()
	b.wg.Wait()
}
