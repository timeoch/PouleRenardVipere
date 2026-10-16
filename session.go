package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/net/websocket"
)

const (
	SnapEvery    = 150       // un instantané toutes les 5 s simulées pour le replay…
	ReplayBudget = 150 << 20 // …espacés davantage au-delà de ~150 Mo d'instantanés
	RoundPause   = 5 * time.Second
)

// snapInterval : écart entre deux instantanés. Les grandes et longues parties
// les espacent pour borner la mémoire du replay ; revenir en arrière rejoue
// alors un peu plus de ticks.
func snapInterval(agents, maxTicks int) int {
	per := agents*(int(unsafe.Sizeof(Agent{}))+8) + 64<<10 // agents, vitesses, état de valeur
	maxSnaps := max(20, ReplayBudget/per)
	return max(SnapEvery, (maxTicks/maxSnaps+SnapEvery-1)/SnapEvery*SnapEvery)
}

// États de l'agent suivi, affichés par l'interface.
const (
	StIdle uint8 = iota
	StChase
	StFlee
	StRest
	StRescue
	StDecoy
	StGuard
	StRoam
	StEscort
	StExhausted
	StPrisoner
	StItem
	StPlayer
)

var speeds = [...]float64{0.25, 0.5, 1, 2, 4}

// ---------------------------------------------------------------------------
// Messages
// ---------------------------------------------------------------------------

type initMessage struct {
	Type          string               `json:"type"`
	Seed          int64                `json:"seed"`
	WorldSize     float64              `json:"world_size"`
	PerSpecies    int                  `json:"per_species"`
	TargetTPS     int                  `json:"target_tps"`
	MaxTicks      int                  `json:"max_ticks"`
	Species       [SpeciesCount]string `json:"species"`
	Camps         [SpeciesCount]Camp   `json:"camps"`
	Obstacles     []Obstacle           `json:"obstacles"`
	Rules         Rules                `json:"rules"`
	GridCols      int                  `json:"grid_cols"`
	Terrain       string               `json:"terrain"` // un chiffre par cellule (TerrPlain…TerrBridge)
	Tunnels       []Tunnel             `json:"tunnels"`
	HeatCols      int                  `json:"heat_cols"`
	NightPeriod   int                  `json:"night_period"`
	NightDuration int                  `json:"night_duration"`
	ReversalTick  int                  `json:"reversal_tick"`
	StockholmSec  int                  `json:"stockholm_sec"`
	Generation    int                  `json:"generation"`
	Evolve        bool                 `json:"evolve"`
	DefaultGenome Genome               `json:"default_genome"`
	DefaultTuning Tuning               `json:"default_tuning"`
}

// FocusInfo détaille l'agent suivi par l'interface.
type FocusInfo struct {
	I         int    `json:"i"`
	Species   uint8  `json:"species"`
	Role      uint8  `json:"role"`
	Stamina   int16  `json:"stamina"`
	Immune    int16  `json:"immune"`
	Boost     uint8  `json:"boost"`
	Prisoner  bool   `json:"prisoner"`
	Exhausted bool   `json:"exhausted"`
	JailTicks uint16 `json:"jail_ticks"`
	Jail      uint8  `json:"jail"`
	State     uint8  `json:"state"`
	Target    int    `json:"target"`
}

type heatMessage struct {
	Captures []uint32 `json:"captures"`
	Rescues  []uint32 `json:"rescues"`
}

type frameMessage struct {
	Type       string                          `json:"type"`
	Tick       int                             `json:"tick"`
	Head       int                             `json:"head"` // tick le plus avancé de la ligne de temps
	TPS        int                             `json:"tps"`
	TickMs     float64                         `json:"tick_ms"`
	Speed      float64                         `json:"speed"`
	Paused     bool                            `json:"paused"`
	Finished   bool                            `json:"finished"`
	Winner     int                             `json:"winner"`
	Night      bool                            `json:"night"`
	Reversed   bool                            `json:"reversed"`
	Seek       bool                            `json:"seek,omitempty"`
	NextRound  int                             `json:"next_round,omitempty"` // secondes avant la manche suivante
	Stats      [SpeciesCount]SpeciesStats      `json:"stats"`
	Held       [SpeciesCount][SpeciesCount]int `json:"held"` // held[camp][espèce]
	Items      []Item                          `json:"items"`
	Player     int                             `json:"player"`
	Focus      *FocusInfo                      `json:"focus,omitempty"`
	Events     []Event                         `json:"events,omitempty"`
	Heat       *heatMessage                    `json:"heat,omitempty"`
	ObsChanged bool                            `json:"obs_changed,omitempty"`
	Obstacles  []Obstacle                      `json:"obstacles,omitempty"`
	Tuning     Tuning                          `json:"tuning"`
	Genomes    [SpeciesCount]Genome            `json:"genomes"`
	// Agents : triplets [x, y, code] à plat. code = espèce (bits 0-1)
	// | rôle << 2 | prisonnier << 4 | épuisé << 5 | immunisé << 6 | sprint gratuit << 7.
	// Absents de l'en-tête JSON des trames binaires (ils suivent en int16).
	Agents []int16 `json:"agents,omitempty"`
}

// ---------------------------------------------------------------------------
// Trames binaires
// ---------------------------------------------------------------------------

// jsonFrames : trames en JSON texte, format et envoi d'origine (-trames json).
var jsonFrames bool

// framePool recycle les tampons des trames binaires entre la boucle de la
// partie, qui les remplit, et l'envoyeur, qui les rend après l'envoi (ou la
// boucle, quand une trame pas encore partie est remplacée par la suivante).
var framePool = sync.Pool{New: func() any {
	b := make([]byte, 0, 64<<10)
	return &b
}}

// frameEncoder écrit les trames binaires d'une session. L'en-tête JSON passe
// par un tampon et un encodeur réutilisés d'une trame à l'autre.
type frameEncoder struct {
	hdr bytes.Buffer
	enc *json.Encoder
}

func newFrameEncoder() *frameEncoder {
	e := &frameEncoder{}
	e.enc = json.NewEncoder(&e.hdr)
	return e
}

// appendBinary ajoute la trame f à dst :
//
//	[0:4]   longueur n de l'en-tête JSON (uint32 petit-boutiste, n pair)
//	[4:4+n] en-tête JSON : tout sauf les agents
//	[4+n:]  agents : triplets int16 petit-boutistes (x, y, code)
//
// Les agents sont copiés tels qu'ils sont en mémoire (amd64 et arm64 sont
// petit-boutistes) : 6 octets par agent au lieu de ~14 caractères en JSON, et
// le navigateur les lit avec un Int16Array posé sur le tampon, sans décodage.
func (e *frameEncoder) appendBinary(dst []byte, f *frameMessage) ([]byte, error) {
	agents := f.Agents
	f.Agents = nil
	e.hdr.Reset()
	err := e.enc.Encode(f)
	f.Agents = agents
	if err != nil {
		return dst, err
	}
	if e.hdr.Len()%2 == 1 {
		e.hdr.WriteByte(' ') // Int16Array exige un décalage pair
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(e.hdr.Len()))
	dst = append(dst, e.hdr.Bytes()...)
	if len(agents) > 0 {
		dst = append(dst, unsafe.Slice((*byte)(unsafe.Pointer(&agents[0])), 2*len(agents))...)
	}
	return dst, nil
}

type tournamentMessage struct {
	Type string `json:"type"`
	ID   int    `json:"id"`
	Tally
	Running bool        `json:"running"`
	Last    *GameResult `json:"last,omitempty"`
}

// ---------------------------------------------------------------------------
// Session : une partie par connexion, avec sa ligne de temps
// ---------------------------------------------------------------------------

type inputEvent struct {
	Tick int
	Cmd  Command
}

type session struct {
	cfg    Config
	evolve bool
	gen    int
	w      *World

	snaps     []*Snapshot  // instantanés croissants, le premier au tick 0
	snapEvery int          // écart entre deux instantanés, en ticks
	inputs    []inputEvent // entrées de simulation, triées par tick
	next      int          // prochaine entrée à rejouer
	head      int

	paused    bool
	speed     float64
	acc       float64
	heat      bool
	heatTick  int
	lastEvent uint64
	seekFlag  bool
	roundAt   time.Time

	tCancel context.CancelFunc
	tID     int
}

func newSession(cfg Config, evolve bool) *session {
	s := &session{cfg: cfg, evolve: evolve, speed: 1}
	s.reset()
	return s
}

func (s *session) reset() {
	s.w = NewWorldWith(s.cfg)
	s.snaps = []*Snapshot{s.w.Snapshot()}
	s.snapEvery = snapInterval(len(s.w.Agents), s.w.MaxTicks)
	s.inputs, s.next, s.head = nil, 0, 0
	s.acc, s.lastEvent, s.heatTick = 0, 0, -1000
	s.roundAt = time.Time{}
}

// step joue un tick : entrées enregistrées pour ce tick, puis simulation.
// L'instantané est pris avant les entrées du tick suivant, comme au replay.
func (s *session) step() {
	s.applyLogged()
	s.w.Update()
	s.head = max(s.head, s.w.Tick)
	if s.w.Tick%s.snapEvery == 0 && s.snaps[len(s.snaps)-1].Tick < s.w.Tick {
		s.snaps = append(s.snaps, s.w.Snapshot())
	}
}

func (s *session) applyLogged() {
	for s.next < len(s.inputs) && s.inputs[s.next].Tick <= s.w.Tick {
		s.w.Apply(s.inputs[s.next].Cmd)
		s.next++
	}
}

// input applique une entrée de simulation tout de suite et l'enregistre. Une
// entrée donnée après un retour en arrière ouvre une nouvelle branche : la
// suite enregistrée (entrées et instantanés) est abandonnée.
func (s *session) input(c Command) {
	s.applyLogged() // entrées déjà prévues pour ce tick, dans l'ordre
	if !s.w.Apply(c) {
		return
	}
	t := s.w.Tick
	k := sort.Search(len(s.inputs), func(i int) bool { return s.inputs[i].Tick > t })
	s.inputs = append(s.inputs[:k], inputEvent{t, c})
	s.next = len(s.inputs)
	n := sort.Search(len(s.snaps), func(i int) bool { return s.snaps[i].Tick > t })
	s.snaps = s.snaps[:n]
	s.head = t
	if c.Cmd == "control" && c.I >= 0 {
		s.w.Focus = c.I // on suit l'agent piloté ; on le garde en vue quand on le lâche
	}
}

// seek revient au tick t : dernier instantané avant t, puis simulation
// jusqu'à t en rejouant les entrées enregistrées.
func (s *session) seek(t int) {
	t = min(max(t, 0), s.head)
	k := sort.Search(len(s.snaps), func(i int) bool { return s.snaps[i].Tick > t }) - 1
	if k < 0 {
		return
	}
	s.w.Restore(s.snaps[k])
	s.next = sort.Search(len(s.inputs), func(i int) bool { return s.inputs[i].Tick >= s.w.Tick })
	for s.w.Tick < t && !s.w.Finished {
		s.applyLogged()
		s.w.Update()
	}
	s.paused, s.acc = true, 0
	s.lastEvent = s.w.EventSeq
	s.seekFlag = true
	s.heatTick = -1000
	s.roundAt = time.Time{}
}

// nextRound lance la manche suivante avec des stratégies évoluées.
func (s *session) nextRound() {
	s.cfg.Genomes = Evolve(s.w.Genomes, s.w.Winner, s.cfg.Seed)
	s.cfg.Tuning = s.w.Tuning
	s.cfg.Seed++
	s.gen++
	s.reset()
}

func (s *session) initMessage() initMessage {
	w := s.w
	terrain := make([]byte, GridCells)
	for i, t := range w.Terrain {
		terrain[i] = '0' + t
	}
	return initMessage{
		Type: "init", Seed: w.Seed, WorldSize: WorldSize, PerSpecies: w.PerSpecies,
		TargetTPS: TargetTPS, MaxTicks: w.MaxTicks, Species: speciesNames,
		Camps: w.Camps, Obstacles: w.Obstacles, Rules: w.Rules,
		GridCols: GridCols, Terrain: string(terrain), Tunnels: w.Tunnels, HeatCols: HeatCols,
		NightPeriod: NightPeriod, NightDuration: NightDuration, ReversalTick: w.ReversalTick(),
		StockholmSec: int(StockholmTicks) / TargetTPS, Generation: s.gen, Evolve: s.evolve,
		DefaultGenome: DefaultGenome, DefaultTuning: DefaultTuning,
	}
}

func (s *session) frame(buf []int16, evBuf []Event, tps int, tickMs float64) frameMessage {
	w := s.w
	for i := range w.Agents {
		a := &w.Agents[i]
		code := int16(a.Species) | int16(a.Role)<<2
		if a.Prisoner {
			code |= 1 << 4
		}
		if a.Exhausted {
			code |= 1 << 5
		}
		if a.Immune > 0 {
			code |= 1 << 6
		}
		if a.Boost > 0 {
			code |= 1 << 7
		}
		buf = append(buf, int16(a.X), int16(a.Y), code)
	}
	f := frameMessage{
		Type: "frame", Tick: w.Tick, Head: s.head, TPS: tps, TickMs: math.Round(tickMs*100) / 100,
		Speed: s.speed, Paused: s.paused, Finished: w.Finished, Winner: w.Winner,
		Night: w.IsNight(), Reversed: w.Reversed, Stats: w.Stats, Held: w.held,
		Items: []Item{}, Player: w.Player, Tuning: w.Tuning, Genomes: w.Genomes, Agents: buf,
	}
	for _, it := range w.Items {
		if it.Active {
			f.Items = append(f.Items, it)
		}
	}
	if w.Focus >= 0 {
		a := &w.Agents[w.Focus]
		f.Focus = &FocusInfo{
			I: w.Focus, Species: uint8(a.Species), Role: uint8(a.Role), Stamina: a.Stamina,
			Immune: a.Immune, Boost: a.Boost, Prisoner: a.Prisoner, Exhausted: a.Exhausted,
			JailTicks: a.JailTicks, Jail: uint8(a.Jail), State: w.focusState, Target: w.focusTarget,
		}
	}
	f.Events, s.lastEvent = w.EventsSince(s.lastEvent, evBuf)
	if s.heat && (w.Tick-s.heatTick >= TargetTPS || s.seekFlag) {
		f.Heat = &heatMessage{Captures: w.Heat[0][:], Rescues: w.Heat[1][:]}
		s.heatTick = w.Tick
	}
	if w.TakeObstaclesChanged() {
		f.ObsChanged, f.Obstacles = true, w.Obstacles
	}
	f.Seek, s.seekFlag = s.seekFlag, false
	if !s.roundAt.IsZero() {
		f.NextRound = max(1, int(math.Ceil(time.Until(s.roundAt).Seconds())))
	}
	return f
}

// parseConfig lit la graine, la configuration (règles, réglages, stratégies)
// et le mode évolution passés dans l'URL de connexion.
func parseConfig(q url.Values) (Config, bool) {
	cfg := serverDefaults
	if v, err := strconv.ParseInt(q.Get("seed"), 10, 64); err == nil {
		cfg.Seed = v
	}
	if raw := q.Get("cfg"); raw != "" {
		var c struct {
			Rules      *Rules
			PerSpecies *int
			Minutes    *float64
			Tuning     *Tuning
			Genomes    *[SpeciesCount]Genome
		}
		if json.Unmarshal([]byte(raw), &c) == nil {
			if c.Rules != nil {
				cfg.Rules = *c.Rules
			}
			if c.PerSpecies != nil {
				cfg.PerSpecies = *c.PerSpecies
			}
			if c.Minutes != nil {
				cfg.MaxTicks = MinutesToTicks(*c.Minutes)
			}
			if c.Tuning != nil {
				cfg.Tuning = c.Tuning.Clamp()
			}
			if c.Genomes != nil {
				for sp := range cfg.Genomes {
					cfg.Genomes[sp] = c.Genomes[sp].Clamp()
				}
			}
		}
	}
	return cfg.Clamp(), q.Get("evolve") == "1"
}

func wsHandler() websocket.Handler {
	return func(ws *websocket.Conn) {
		defer ws.Close()
		s := newSession(parseConfig(ws.Request().URL.Query()))

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Envoyeur : les messages de contrôle (init, tournoi) passent toujours
		// et en priorité ; pour les trames, seule la plus récente compte. Si le
		// navigateur lit moins vite que 30 trames/s, les trames intermédiaires
		// sont abandonnées au lieu de bloquer la simulation.
		ctrl := make(chan []byte, 32)
		latest := make(chan *[]byte, 1)
		go func() {
			defer cancel()
			for {
				select {
				case data := <-ctrl:
					if websocket.Message.Send(ws, string(data)) != nil {
						return
					}
					continue
				default:
				}
				var err error
				select {
				case <-ctx.Done():
					return
				case data := <-ctrl:
					err = websocket.Message.Send(ws, string(data))
				case bp := <-latest:
					if jsonFrames {
						err = websocket.Message.Send(ws, string(*bp))
					} else {
						err = websocket.Message.Send(ws, *bp) // []byte : trame binaire, sans copie
						framePool.Put(bp)
					}
				}
				if err != nil {
					return
				}
			}
		}()
		send := func(v any) {
			data, err := json.Marshal(v)
			if err != nil {
				return
			}
			select {
			case ctrl <- data:
			case <-ctx.Done():
			}
		}
		publish := func(bp *[]byte) {
			select {
			case old := <-latest: // trame précédente pas encore envoyée : on la remplace
				if !jsonFrames {
					framePool.Put(old)
				}
			default:
			}
			latest <- bp
		}
		dropFrame := func() {
			select {
			case old := <-latest:
				if !jsonFrames {
					framePool.Put(old)
				}
			default:
			}
		}

		// Lecteur : commandes JSON du navigateur, traitées par la boucle.
		cmds := make(chan Command, 64)
		go func() {
			defer cancel()
			for {
				var msg string
				if err := websocket.Message.Receive(ws, &msg); err != nil {
					return
				}
				var c Command
				if json.Unmarshal([]byte(msg), &c) != nil {
					continue
				}
				select {
				case cmds <- c:
				case <-ctx.Done():
					return
				}
			}
		}()

		send(s.initMessage())

		ticker := time.NewTicker(time.Second / time.Duration(TargetTPS))
		defer ticker.Stop()

		ticks, tps := 0, 0
		lastCheck := time.Now()
		tickMs := 0.0
		buf := make([]int16, 0, len(s.w.Agents)*3)
		var evBuf []Event
		fe := newFrameEncoder()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

		drain:
			for {
				select {
				case c := <-cmds:
					s.handle(ctx, c, send)
				default:
					break drain
				}
			}

			w := s.w
			if !s.paused && !w.Finished {
				s.acc += s.speed
				for s.acc >= 1 && !w.Finished {
					t0 := time.Now()
					s.step()
					ms := float64(time.Since(t0).Microseconds()) / 1000.0
					if tickMs == 0 {
						tickMs = ms
					} else {
						tickMs = 0.9*tickMs + 0.1*ms
					}
					ticks++
					s.acc--
				}
			}

			// Évolution : la manche suivante démarre quelques secondes après la fin.
			if s.evolve && w.Finished && w.Tick == s.head {
				switch {
				case s.roundAt.IsZero():
					s.roundAt = time.Now().Add(RoundPause)
				case time.Now().After(s.roundAt):
					s.nextRound()
					dropFrame()
					send(s.initMessage())
				}
			}

			if time.Since(lastCheck) >= time.Second {
				tps, ticks = ticks, 0
				lastCheck = time.Now()
			}

			frame := s.frame(buf[:0], evBuf[:0], tps, tickMs)
			buf, evBuf = frame.Agents, frame.Events
			if jsonFrames {
				data, err := json.Marshal(frame)
				if err != nil {
					return
				}
				publish(&data)
				continue
			}
			bp := framePool.Get().(*[]byte)
			data, err := fe.appendBinary((*bp)[:0], &frame)
			if err != nil {
				return
			}
			*bp = data
			publish(bp)
		}
	}
}

func (s *session) handle(ctx context.Context, c Command, send func(any)) {
	switch c.Cmd {
	case "pause":
		s.paused = true
	case "resume":
		s.paused = false
	case "speed":
		for _, v := range speeds {
			if c.Value == v {
				s.speed = v
			}
		}
	case "seek":
		s.seek(c.Tick)
	case "follow":
		if c.I >= -1 && c.I < len(s.w.Agents) {
			s.w.Focus, s.w.focusState, s.w.focusTarget = c.I, StIdle, -1
		}
	case "heat":
		s.heat, s.heatTick = c.On, -1000
	case "evolve":
		s.evolve = c.On
		if !c.On {
			s.roundAt = time.Time{}
		}
	case "tournament":
		s.tournament(ctx, c.N, send)
	default:
		s.input(c)
	}
}

// tournament lance (n > 0) ou arrête (n ≤ 0) un tournoi en arrière-plan,
// avec les règles, réglages et stratégies de la partie en cours.
func (s *session) tournament(ctx context.Context, n int, send func(any)) {
	if s.tCancel != nil {
		s.tCancel()
		s.tCancel = nil
	}
	s.tID++
	id := s.tID
	if n <= 0 {
		send(tournamentMessage{Type: "tournament", ID: id})
		return
	}
	n = min(n, 500)
	tctx, tcancel := context.WithCancel(ctx)
	s.tCancel = tcancel
	cfg := s.cfg
	cfg.Seed, cfg.Tuning, cfg.Genomes = s.w.Seed, s.w.Tuning, s.w.Genomes
	go func() {
		t := Tally{Total: n}
		send(tournamentMessage{Type: "tournament", ID: id, Tally: t, Running: true})
		RunTournament(tctx, cfg, n, func(r GameResult) {
			t.Add(r)
			send(tournamentMessage{Type: "tournament", ID: id, Tally: t, Running: t.Done < t.Total, Last: &r})
		})
	}()
}
