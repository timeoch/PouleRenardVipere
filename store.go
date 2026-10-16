package main

import (
	"container/list"
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Persistance des tournois dans PostgreSQL (option -db)
// ---------------------------------------------------------------------------

// Une ligne par partie dans runs, et la télémétrie de chaque tick dans
// run_ticks : agents libres, prisonniers et épuisés de chaque espèce. Une
// partie de 5 minutes donne 9 000 ticks × 3 espèces = 27 000 lignes.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS runs (
	id          bigserial   PRIMARY KEY,
	seed        bigint      NOT NULL,
	rules       text        NOT NULL,
	per_species integer     NOT NULL,
	winner      smallint    NOT NULL,
	ticks       integer     NOT NULL,
	created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS run_ticks (
	run_id    bigint   NOT NULL,
	tick      integer  NOT NULL,
	species   smallint NOT NULL,
	free      integer  NOT NULL,
	prisoners integer  NOT NULL,
	exhausted integer  NOT NULL
);`

// L'index composite est à part : le rapport compare la même requête avec et
// sans lui (EXPLAIN ANALYZE).
const (
	indexSQL     = `CREATE INDEX IF NOT EXISTS run_ticks_run_tick ON run_ticks (run_id, tick)`
	dropIndexSQL = `DROP INDEX IF EXISTS run_ticks_run_tick`
	curveSQL     = `SELECT tick, species, free, prisoners, exhausted FROM run_ticks
		WHERE run_id = $1 AND tick BETWEEN $2 AND $3 ORDER BY tick`
)

// TickRow : télémétrie d'une espèce à un tick.
type TickRow struct {
	Tick      int32 `json:"t"`
	Species   int16 `json:"s"`
	Free      int32 `json:"free"`
	Prisoners int32 `json:"prisoners"`
	Exhausted int32 `json:"exhausted"`
}

// appendTelemetry ajoute l'état du tick courant (une ligne par espèce).
func appendTelemetry(rows []TickRow, w *World) []TickRow {
	for sp := range w.Stats {
		st := &w.Stats[sp]
		rows = append(rows, TickRow{Tick: int32(w.Tick), Species: int16(sp),
			Free: int32(st.Free), Prisoners: int32(st.Prisoners), Exhausted: int32(st.Exhausted)})
	}
	return rows
}

type curveKey struct {
	run      int64
	from, to int
}

// Store : pool de connexions et cache des courbes déjà lues.
type Store struct {
	pool  *pgxpool.Pool
	cache *lruCache
}

// OpenStore ouvre le pool. Taille maximale : 2 connexions par cœur, comme la
// règle (cœurs × 2) + disques ; au-delà, PostgreSQL passe son temps en
// bascules de contexte au lieu de répondre.
func OpenStore(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = int32(2*runtime.NumCPU() + 1)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	s := &Store{pool: pool, cache: newLRU(256)}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

// SaveRun enregistre une partie et sa télémétrie. Les lignes partent en un
// seul COPY (protocole binaire de PostgreSQL) au lieu d'un INSERT par ligne :
// un aller-retour réseau et une transaction pour 27 000 lignes.
func (s *Store) SaveRun(ctx context.Context, cfg Config, r GameResult, rows []TickRow) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO runs (seed, rules, per_species, winner, ticks) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		r.Seed, cfg.Rules.String(), cfg.PerSpecies, r.Winner, r.Ticks).Scan(&id)
	if err != nil {
		return 0, err
	}
	_, err = s.pool.CopyFrom(ctx, pgx.Identifier{"run_ticks"},
		[]string{"run_id", "tick", "species", "free", "prisoners", "exhausted"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			t := &rows[i]
			return []any{id, t.Tick, t.Species, t.Free, t.Prisoners, t.Exhausted}, nil
		}))
	return id, err
}

// Curve renvoie la télémétrie d'une partie entre deux ticks. Cache-aside :
// une courbe déjà lue sort de la mémoire sans requête SQL.
func (s *Store) Curve(ctx context.Context, run int64, from, to int) ([]TickRow, error) {
	k := curveKey{run, from, to}
	if rows, ok := s.cache.get(k); ok {
		return rows, nil
	}
	rows, err := s.curveDB(ctx, run, from, to)
	if err != nil {
		return nil, err
	}
	s.cache.put(k, rows)
	return rows, nil
}

func (s *Store) curveDB(ctx context.Context, run int64, from, to int) ([]TickRow, error) {
	r, err := s.pool.Query(ctx, curveSQL, run, from, to)
	if err != nil {
		return nil, err
	}
	rows := make([]TickRow, 0, 3*(to-from+1))
	for r.Next() {
		var t TickRow
		if err := r.Scan(&t.Tick, &t.Species, &t.Free, &t.Prisoners, &t.Exhausted); err != nil {
			r.Close()
			return nil, err
		}
		rows = append(rows, t)
	}
	r.Close()
	return rows, r.Err()
}

// curveHandler : GET /api/courbe?run=ID&de=0&a=9000
func (s *Store) curveHandler(rw http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	run, err := strconv.ParseInt(q.Get("run"), 10, 64)
	if err != nil {
		http.Error(rw, "run manquant", http.StatusBadRequest)
		return
	}
	from, _ := strconv.Atoi(q.Get("de"))
	to, err := strconv.Atoi(q.Get("a"))
	if err != nil {
		to = MaxMinutes * 60 * TargetTPS
	}
	rows, err := s.Curve(r.Context(), run, from, to)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(rows)
}

// ---------------------------------------------------------------------------
// Cache LRU
// ---------------------------------------------------------------------------

// lruCache : borné en nombre d'entrées (pas de croissance sans fin), O(1) en
// lecture comme en écriture grâce à la table de hachage et à la liste
// doublement chaînée (tête = entrée la plus récente). Le mutex protège les
// requêtes HTTP concurrentes ; la section critique ne fait que déplacer un
// maillon.
type lruCache struct {
	mu    sync.Mutex
	max   int
	items map[curveKey]*list.Element
	order *list.List
}

type lruEntry struct {
	key  curveKey
	rows []TickRow
}

func newLRU(max int) *lruCache {
	return &lruCache{max: max, items: make(map[curveKey]*list.Element, max), order: list.New()}
}

func (c *lruCache) get(k curveKey) ([]TickRow, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*lruEntry).rows, true
}

func (c *lruCache) put(k curveKey, rows []TickRow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		e.Value.(*lruEntry).rows = rows
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&lruEntry{k, rows})
	if c.order.Len() > c.max {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(*lruEntry).key)
	}
}
