package main

import (
	_ "embed"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
)

// ---------------------------------------------------------------------------
// Paramètres de la simulation
// ---------------------------------------------------------------------------

const (
	MasterSeed   int64   = 42
	Port         int     = 8081
	WorldSize    float64 = 5000.0
	GridCols     int     = 100
	GridRows     int     = 100
	GridCells            = GridCols * GridRows
	CellSize             = WorldSize / float64(GridCols) // 50 unités
	SpeciesCount         = 3
	TargetTPS    int     = 30

	// Effectif et durée d'une manche, réglables au lancement.
	DefaultPerSpecies int = 3000 // individus par espèce
	MinPerSpecies     int = 100
	MaxPerSpecies     int = 10000
	DefaultMinutes    int = 5 // durée d'une manche en minutes simulées
	MaxMinutes        int = 30

	ObstacleCount int     = 40
	MaxObstacles  int     = 120 // plafond, haies posées pendant la partie comprises
	WallMargin    float64 = 20.0

	CampRadius        float64 = 230.0  // zone refuge de chaque camp
	SpawnRadius       float64 = 1300.0 // dispersion initiale autour du camp
	VisionRadius      float64 = 420.0  // portée de repérage d'une proie
	FleeRadius        float64 = 220.0  // un prédateur plus proche déclenche la fuite
	CaptureRadius     float64 = 14.0
	RescueRadius      float64 = 18.0
	GuardRadius       float64 = 700.0 // zone défendue par les gardiens autour de leur camp
	HomeRetreatRadius float64 = 900.0 // en deçà, on fuit ou on récupère vers son camp
	PackRadius        float64 = 100.0 // un congénère aussi proche suffit pour chasser en tenaille
	DecoyRange        float64 = 950.0 // distance à la prison où un leurre cherche les gardiens
	DecoyTaunt        float64 = 250.0 // distance de provocation d'un leurre
	LOSStep           float64 = 40.0  // pas d'échantillonnage des lignes de vue (haie la plus fine : 60)

	ChaseFactor = 1.40 // multiplicateur de vitesse en poursuite
	FleeFactor  = 1.30 // multiplicateur de vitesse en fuite
	WalkFactor  = 0.55 // multiplicateur de vitesse en marche
	BoostFactor = 1.15 // bonus de vitesse en sprint gratuit (frénésie, plume)

	GuardRatio   = 0.08 // 8 % de gardiens
	RescuerRatio = 0.15 // 15 % de libérateurs
	DecoyShare   = 0.30 // part des libérateurs qui font diversion (règle « diversion »)

	MaxStamina          int16 = 100
	StaminaDrainSprint  int16 = 1
	StaminaRecoveryWalk int16 = 1
	StaminaRecoveryCamp int16 = 3
	StaminaResumeSprint int16 = 50 // sortie de l'épuisement (~1,7 s immobile)
	StaminaLowThreshold int16 = 25 // seuil par défaut sous lequel l'agent cesse de chasser (ténacité 50 %)
	StaminaRestUntil    int16 = 80
	ImmunityTicks       int16 = 120  // 4 s de retour escorté après une libération
	EscortFactor              = 1.10 // allure du retour escorté (sans coût d'endurance)
)

// ---------------------------------------------------------------------------
// Espèces, rôles, agents
// ---------------------------------------------------------------------------

type Species uint8

const (
	Poule Species = iota
	Renard
	Vipere
)

var speciesNames = [SpeciesCount]string{"Poules", "Renards", "Vipères"}

// PreyOf : la poule mange la vipère, le renard mange la poule, la vipère mord le renard.
func PreyOf(s Species) Species { return (s + 2) % SpeciesCount }

// PredatorOf : inverse de PreyOf.
func PredatorOf(s Species) Species { return (s + 1) % SpeciesCount }

type Role uint8

const (
	Chasseur   Role = iota // chasse ses proies, erre vers leur centre de gravité
	Liberateur             // va délivrer ses coéquipiers prisonniers
	Gardien                // défend son camp et les prisonniers qu'il détient
	Leurre                 // attire les gardiens adverses loin de leur prison
)

// Agent tient dans une ligne de cache de 64 octets (vérifié par les tests).
type Agent struct {
	X, Y      float64
	VX, VY    float64 // vitesse décidée pour le tick courant
	Speed     float64
	WanderAng float64
	Stamina   int16
	Immune    int16  // ticks d'immunité restants
	JailTicks uint16 // ticks passés en prison (syndrome de Stockholm)
	Species   Species
	Role      Role
	Jail      Species // camp où l'agent est détenu
	Boost     uint8   // ticks de sprint gratuit restants (frénésie, plume)
	Prisoner  bool
	Exhausted bool
	Resting   bool
}

type Obstacle struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type Camp struct {
	Name string  `json:"name"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	R    float64 `json:"r"`
}

func (c *Camp) Contains(x, y float64) bool {
	dx, dy := x-c.X, y-c.Y
	return dx*dx+dy*dy <= c.R*c.R
}

type SpeciesStats struct {
	Free      int `json:"free"`
	Prisoners int `json:"prisoners"`
	Exhausted int `json:"exhausted"`
	Captures  int `json:"captures"` // captures réalisées par l'espèce
	Rescues   int `json:"rescues"`  // libérations réalisées par l'espèce
	Escapes   int `json:"escapes"`  // évasions de membres de l'espèce
	Recruits  int `json:"recruits"` // captifs passés dans l'espèce (syndrome de Stockholm)
}

// ---------------------------------------------------------------------------
// Grille spatiale 1D contiguë (même principe que ZombieHorde)
// ---------------------------------------------------------------------------

// Cell décrit la plage [Start, Start+Count) de Items occupée par la cellule.
type Cell struct {
	Start int32
	Count int32
}

// SpatialGrid range les indices d'un sous-ensemble d'agents par cellule de
// 50×50 dans une seule tranche contiguë (tri par comptage, O(N), 0 alloc en
// régime permanent). colPrefix permet de savoir en O(1) si un segment vertical
// de cellules est vide ; Cell.Start joue ce rôle pour les segments horizontaux.
type SpatialGrid struct {
	Cells     [GridCells]Cell
	Items     []int32
	cellOf    []int32
	colPrefix [GridCols * (GridRows + 1)]int32
}

func cellCoord(v float64, n int) int {
	c := int(v / CellSize)
	if c < 0 {
		return 0
	}
	if c >= n {
		return n - 1
	}
	return c
}

func CellIndex(x, y float64) int {
	return cellCoord(y, GridRows)*GridCols + cellCoord(x, GridCols)
}

// Build indexe les agents de l'espèce sp, libres (prisoners=false) ou
// prisonniers (prisoners=true). Les indices d'une cellule sont croissants.
func (g *SpatialGrid) Build(agents []Agent, sp Species, prisoners bool) {
	n := len(agents)
	if cap(g.cellOf) < n {
		g.cellOf = make([]int32, n)
		g.Items = make([]int32, n)
	}
	g.cellOf = g.cellOf[:n]
	g.Items = g.Items[:n]
	for i := range g.Cells {
		g.Cells[i] = Cell{}
	}

	active := 0
	for i := range agents {
		a := &agents[i]
		if a.Species != sp || a.Prisoner != prisoners {
			g.cellOf[i] = -1
			continue
		}
		c := int32(CellIndex(a.X, a.Y))
		g.cellOf[i] = c
		g.Cells[c].Count++
		active++
	}

	var start int32
	for i := range g.Cells {
		g.Cells[i].Start = start
		start += g.Cells[i].Count
		g.Cells[i].Count = 0
	}
	for i := 0; i < n; i++ {
		c := g.cellOf[i]
		if c < 0 {
			continue
		}
		cell := &g.Cells[c]
		g.Items[cell.Start+cell.Count] = int32(i)
		cell.Count++
	}
	g.Items = g.Items[:active]

	for cx := 0; cx < GridCols; cx++ {
		base := cx * (GridRows + 1)
		var acc int32
		g.colPrefix[base] = 0
		for cy := 0; cy < GridRows; cy++ {
			acc += g.Cells[cy*GridCols+cx].Count
			g.colPrefix[base+cy+1] = acc
		}
	}
}

// RowItems : agents des cellules cx0…cx1 de la rangée cy. Les cellules d'une
// rangée se suivent dans Items, d'où une seule tranche contiguë.
func (g *SpatialGrid) RowItems(cy, cx0, cx1 int) []int32 {
	a := &g.Cells[cy*GridCols+cx0]
	b := &g.Cells[cy*GridCols+cx1]
	return g.Items[a.Start : b.Start+b.Count]
}

func (g *SpatialGrid) CellRange(x, y, r float64) (cx0, cy0, cx1, cy1 int) {
	return cellCoord(x-r, GridCols), cellCoord(y-r, GridRows), cellCoord(x+r, GridCols), cellCoord(y+r, GridRows)
}

func (g *SpatialGrid) rowCount(cy, xl, xr int) int32 {
	a := &g.Cells[cy*GridCols+xl]
	b := &g.Cells[cy*GridCols+xr]
	return b.Start + b.Count - a.Start
}

func (g *SpatialGrid) colCount(cx, yt, yd int) int32 {
	base := cx * (GridRows + 1)
	return g.colPrefix[base+yd+1] - g.colPrefix[base+yt]
}

// Nearest retourne l'agent indexé le plus proche de (x, y), à une distance
// ≤ maxDist, accepté par accept ; -1 s'il n'y en a pas. En cas d'égalité
// stricte de distance, le plus petit indice gagne : le résultat est identique
// à un scan linéaire. Exploration par anneaux de cellules croissants ; les
// segments d'anneau vides sont sautés en O(1).
func (g *SpatialGrid) Nearest(agents []Agent, x, y, maxDist float64, accept func(*Agent) bool) (int, float64) {
	cx0 := cellCoord(x, GridCols)
	cy0 := cellCoord(y, GridRows)
	bestIdx := -1
	best2 := maxDist * maxDist

	scan := func(c int) {
		cell := &g.Cells[c]
		for _, j := range g.Items[cell.Start : cell.Start+cell.Count] {
			b := &agents[j]
			dx := b.X - x
			dy := b.Y - y
			d2 := dx*dx + dy*dy
			if d2 > best2 || (d2 == best2 && bestIdx >= 0 && int(j) > bestIdx) {
				continue
			}
			if !accept(b) {
				continue
			}
			best2 = d2
			bestIdx = int(j)
		}
	}

	for k := 0; ; k++ {
		if k > 0 {
			ringMin := float64(k-1) * CellSize // distance minimale à l'anneau k
			if ringMin*ringMin > best2 {
				break
			}
		}
		xa, xb, ya, yb := cx0-k, cx0+k, cy0-k, cy0+k
		if xa < 0 && ya < 0 && xb >= GridCols && yb >= GridRows {
			break
		}
		if k == 0 {
			scan(cy0*GridCols + cx0)
			continue
		}
		xl, xr := max(xa, 0), min(xb, GridCols-1)
		if ya >= 0 && g.rowCount(ya, xl, xr) > 0 {
			for cx := xl; cx <= xr; cx++ {
				scan(ya*GridCols + cx)
			}
		}
		if yb < GridRows && g.rowCount(yb, xl, xr) > 0 {
			for cx := xl; cx <= xr; cx++ {
				scan(yb*GridCols + cx)
			}
		}
		yt, yd := max(ya+1, 0), min(yb-1, GridRows-1)
		if yt > yd {
			continue
		}
		if xa >= 0 && g.colCount(xa, yt, yd) > 0 {
			for cy := yt; cy <= yd; cy++ {
				scan(cy*GridCols + xa)
			}
		}
		if xb < GridCols && g.colCount(xb, yt, yd) > 0 {
			for cy := yt; cy <= yd; cy++ {
				scan(cy*GridCols + xb)
			}
		}
	}
	if bestIdx < 0 {
		return -1, 0
	}
	return bestIdx, math.Sqrt(best2)
}

// ---------------------------------------------------------------------------
// Monde
// ---------------------------------------------------------------------------

// worldState regroupe l'état mutable de type valeur. Un instantané du replay
// en est une simple copie, complétée par les tranches (agents, haies).
type worldState struct {
	Tick     int
	Stats    [SpeciesCount]SpeciesStats
	Finished bool
	Winner   int // espèce gagnante, -1 si égalité
	Reversed bool
	Tuning   Tuning
	Genomes  [SpeciesCount]Genome
	Player   int // agent piloté au clavier, -1 sinon
	Input    PlayerInput
	Items    [MaxItems]Item
	Heat     [2][HeatCols * HeatCols]uint32 // captures, libérations

	pcg        rand.PCG
	jailSeq    [SpeciesCount]int32
	leader     int
	eliminated [SpeciesCount]bool
	heldPeak   [SpeciesCount]int // pic de captifs par camp depuis la dernière vidange
	jailMark   [SpeciesCount]int // dernier palier de captifs annoncé par camp
	unguarded  [SpeciesCount]bool
	unguardAt  [SpeciesCount]int
	convAcc    [SpeciesCount][SpeciesCount]int // passages Stockholm pas encore annoncés
}

// World contient tout l'état d'une partie. Chaque connexion WebSocket a son
// propre monde : deux onglets ne se perturbent pas.
type World struct {
	worldState
	Seed       int64
	Rules      Rules
	PerSpecies int // individus par espèce au départ
	MaxTicks   int // durée de la manche en ticks
	Agents     []Agent
	Obstacles  []Obstacle
	Camps      [SpeciesCount]Camp
	Terrain    [GridCells]uint8
	Tunnels    []Tunnel
	Focus      int // agent suivi par l'interface (-1 : aucun), sans effet sur la partie
	EventSeq   uint64

	rng       *rand.Rand
	free      [SpeciesCount]SpatialGrid // agents libres de chaque espèce
	jail      [SpeciesCount]SpatialGrid // prisonniers de chaque espèce
	obsCells  [GridCells]Cell           // index des obstacles par cellule
	obsItems  []int16
	cellFree  [GridCells]bool
	campFlow  [SpeciesCount]FlowField // chemin vers chaque camp
	preyFlow  [SpeciesCount]FlowField // chemin vers le centre de gravité de chaque espèce
	flowQueue []int32
	centerX   [SpeciesCount]float64 // centre de gravité des agents libres
	centerY   [SpeciesCount]float64
	prevV     []float32                       // vitesse du tick précédent (anticipation)
	held      [SpeciesCount][SpeciesCount]int // held[camp][espèce] : captifs
	visionR   [SpeciesCount]float64
	fleeR     [SpeciesCount]float64
	restThr   [SpeciesCount]int16
	qx, qy    float64 // position de la requête en cours (hautes herbes)
	self      *Agent  // agent qui interroge la grille (chasse en meute)
	events    [EventRing]Event
	obsDirty  bool

	focusState  uint8
	focusTarget int

	vulnFn     func(*Agent) bool
	seeFn      func(*Agent) bool
	guardFn    func(*Agent) bool
	prisonerFn func(*Agent) bool
	awakeFn    func(*Agent) bool
	guardianFn func(*Agent) bool
	packFn     func(*Agent) bool
}

// NewWorld crée une partie avec la configuration par défaut (toutes les règles).
func NewWorld(seed int64) *World { return NewWorldWith(DefaultConfig(seed)) }

func NewWorldWith(cfg Config) *World {
	cfg = cfg.Clamp()
	w := &World{Seed: cfg.Seed, Rules: cfg.Rules, PerSpecies: cfg.PerSpecies, MaxTicks: cfg.MaxTicks, Focus: -1, focusTarget: -1}
	w.Winner, w.Player, w.leader = -1, -1, -1
	w.Tuning, w.Genomes = cfg.Tuning, cfg.Genomes
	w.pcg = *rand.NewPCG(uint64(cfg.Seed), 0x5eed)
	w.rng = rand.New(&w.pcg)
	w.vulnFn = w.isVulnerable
	w.seeFn = w.canSee
	w.guardFn = w.isGuardTarget
	w.prisonerFn = func(b *Agent) bool { return b.Prisoner }
	w.awakeFn = func(b *Agent) bool { return !b.Prisoner && !b.Exhausted }
	w.guardianFn = func(b *Agent) bool { return !b.Prisoner && !b.Exhausted && b.Role == Gardien }
	w.packFn = func(b *Agent) bool { return b != w.self && !b.Exhausted }
	total := cfg.PerSpecies * SpeciesCount
	w.prevV = make([]float32, 2*total)
	w.flowQueue = make([]int32, 0, GridCells)

	// Camps aux sommets d'un triangle équilatéral : chaque espèce est à la
	// même distance de sa proie et de son prédateur.
	const campTop, campBottom = 700.0, 4200.0
	half := (campBottom - campTop) / math.Sqrt(3)
	w.Camps = [SpeciesCount]Camp{
		{Name: "Poulailler", X: WorldSize / 2, Y: campTop, R: CampRadius},
		{Name: "Terrier", X: WorldSize/2 - half, Y: campBottom, R: CampRadius},
		{Name: "Nid", X: WorldSize/2 + half, Y: campBottom, R: CampRadius},
	}
	if w.Rules.Terrain {
		w.genTerrain()
	}

	// Haies et rochers procéduraux, à distance des camps et des rivières.
	for attempts := 0; len(w.Obstacles) < ObstacleCount && attempts < 2000; attempts++ {
		ow := 120.0 + w.rng.Float64()*320.0
		oh := 60.0 + w.rng.Float64()*180.0
		if w.rng.Float64() > 0.5 {
			ow, oh = oh, ow
		}
		ox := 150.0 + w.rng.Float64()*(WorldSize-ow-300.0)
		oy := 150.0 + w.rng.Float64()*(WorldSize-oh-300.0)
		if !w.rectNearCamp(ox, oy, ow, oh, 250) && !w.rectTouchesWater(ox, oy, ow, oh) {
			w.Obstacles = append(w.Obstacles, Obstacle{X: ox, Y: oy, W: ow, H: oh})
		}
	}
	w.indexObstacles()
	w.computeFreeCells()
	if w.Rules.Bonuses {
		w.genTunnels()
	}

	// Espèces mélangées dans le tableau pour qu'aucune ne soit avantagée par
	// l'ordre de traitement.
	order := make([]Species, total)
	for i := range order {
		order[i] = Species(i / cfg.PerSpecies)
	}
	w.rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })

	w.Agents = make([]Agent, total)
	for i, sp := range order {
		camp := &w.Camps[sp]
		var x, y float64
		for {
			ang := w.rng.Float64() * 2 * math.Pi
			r := CampRadius + 60 + math.Sqrt(w.rng.Float64())*(SpawnRadius-CampRadius-60)
			x, y = camp.X+math.Cos(ang)*r, camp.Y+math.Sin(ang)*r
			if !w.Blocked(x, y) {
				break
			}
		}
		w.Agents[i] = Agent{
			X:         x,
			Y:         y,
			Speed:     2.2 + w.rng.Float64()*0.6,
			WanderAng: w.rng.Float64() * 2 * math.Pi,
			Stamina:   MaxStamina - int16(w.rng.IntN(20)),
			Species:   sp,
		}
	}

	w.refreshStats()
	w.assignRoles()
	w.buildCampFlows()
	w.buildPreyFlows()
	for sp := Species(0); sp < SpeciesCount; sp++ {
		w.jail[sp].Build(w.Agents, sp, true)
	}
	return w
}

func (w *World) indexObstacles() {
	for i := range w.obsCells {
		w.obsCells[i] = Cell{}
	}
	forCells := func(o *Obstacle, fn func(c int)) {
		for cy := cellCoord(o.Y, GridRows); cy <= cellCoord(o.Y+o.H, GridRows); cy++ {
			for cx := cellCoord(o.X, GridCols); cx <= cellCoord(o.X+o.W, GridCols); cx++ {
				fn(cy*GridCols + cx)
			}
		}
	}
	total := int32(0)
	for k := range w.Obstacles {
		forCells(&w.Obstacles[k], func(c int) { w.obsCells[c].Count++; total++ })
	}
	var start int32
	for i := range w.obsCells {
		w.obsCells[i].Start = start
		start += w.obsCells[i].Count
		w.obsCells[i].Count = 0
	}
	w.obsItems = make([]int16, total)
	for k := range w.Obstacles {
		forCells(&w.Obstacles[k], func(c int) {
			cell := &w.obsCells[c]
			w.obsItems[cell.Start+cell.Count] = int16(k)
			cell.Count++
		})
	}
}

// Blocked : murs d'enceinte, eau ou obstacle. Seuls les obstacles qui
// recouvrent la cellule du point sont testés.
func (w *World) Blocked(x, y float64) bool {
	if x <= WallMargin || x >= WorldSize-WallMargin || y <= WallMargin || y >= WorldSize-WallMargin {
		return true
	}
	ci := CellIndex(x, y)
	if w.Terrain[ci] == TerrWater {
		return true
	}
	c := &w.obsCells[ci]
	for _, k := range w.obsItems[c.Start : c.Start+c.Count] {
		o := &w.Obstacles[k]
		if x >= o.X && x <= o.X+o.W && y >= o.Y && y <= o.Y+o.H {
			return true
		}
	}
	return false
}

// lineClear : aucun mur, haie ou cours d'eau entre deux points.
func (w *World) lineClear(x0, y0, x1, y1 float64) bool {
	dx, dy := x1-x0, y1-y0
	n := int(hypot(dx, dy) / LOSStep)
	for k := 1; k <= n; k++ {
		t := float64(k) / float64(n+1)
		if w.Blocked(x0+dx*t, y0+dy*t) {
			return false
		}
	}
	return true
}

// preyOf / predOf tiennent compte du renversement de la chaîne alimentaire.
func (w *World) preyOf(s Species) Species {
	if w.Reversed {
		return PredatorOf(s)
	}
	return PreyOf(s)
}

func (w *World) predOf(s Species) Species {
	if w.Reversed {
		return PreyOf(s)
	}
	return PredatorOf(s)
}

// isVulnerable : libre, non immunisé et hors de son propre camp.
func (w *World) isVulnerable(b *Agent) bool {
	return !b.Prisoner && b.Immune == 0 && !w.Camps[b.Species].Contains(b.X, b.Y)
}

// visible : dans les hautes herbes, un agent n'est repéré que de près.
func (w *World) visible(b *Agent) bool {
	if w.Terrain[CellIndex(b.X, b.Y)] != TerrGrass {
		return true
	}
	dx, dy := b.X-w.qx, b.Y-w.qy
	return dx*dx+dy*dy <= GrassSight*GrassSight
}

func (w *World) canSee(b *Agent) bool { return w.isVulnerable(b) && w.visible(b) }

// isGuardTarget : proie vulnérable et visible qui rôde près du camp de son prédateur.
func (w *World) isGuardTarget(b *Agent) bool {
	if !w.canSee(b) {
		return false
	}
	c := &w.Camps[w.predOf(b.Species)]
	dx, dy := b.X-c.X, b.Y-c.Y
	return dx*dx+dy*dy <= GuardRadius*GuardRadius
}

// ---------------------------------------------------------------------------
// Boucle de simulation
// ---------------------------------------------------------------------------

// Update avance la partie d'un tick.
//
//  1. Préparation : nuit, renversement, centres de gravité, grilles.
//  2. Décision : chaque agent choisit sa vitesse à partir des positions de
//     début de tick (lecture seule des autres agents).
//  3. Déplacement : application des vitesses avec collisions et terriers.
//  4. Bonus ramassés, captures, libérations, évasions, temps de prison.
//  5. Statistiques, rôles, chemins, fin de manche.
func (w *World) Update() {
	if w.Finished {
		return
	}
	w.Tick++
	w.seasons()
	w.prepare()
	for i := range w.Agents {
		w.decide(i)
	}
	for i := range w.Agents {
		w.move(i)
	}
	for sp := Species(0); sp < SpeciesCount; sp++ {
		w.free[sp].Build(w.Agents, sp, false)
	}
	if w.Rules.Bonuses {
		w.pickups()
	}
	w.captures()
	for sp := Species(0); sp < SpeciesCount; sp++ {
		w.jail[sp].Build(w.Agents, sp, true)
	}
	w.rescues()
	if w.Rules.Escape {
		w.escapes()
	}
	w.prisonTime()
	if w.Rules.Bonuses && w.Tick%ItemEvery == 0 {
		w.spawnItem()
	}
	w.refreshStats()
	w.jailEvents()
	if w.Tick%RoleEvery == 0 {
		w.assignRoles()
	}
	if w.Tick%FlowEvery == 0 {
		w.buildPreyFlows()
	}
	w.checkEnd()
}

func (w *World) computeCenters() {
	var sx, sy [SpeciesCount]float64
	var n [SpeciesCount]int
	for i := range w.Agents {
		a := &w.Agents[i]
		if a.Prisoner {
			continue
		}
		sx[a.Species] += a.X
		sy[a.Species] += a.Y
		n[a.Species]++
	}
	for sp := range n {
		w.centerX[sp], w.centerY[sp] = WorldSize/2, WorldSize/2
		if n[sp] > 0 {
			w.centerX[sp] = sx[sp] / float64(n[sp])
			w.centerY[sp] = sy[sp] / float64(n[sp])
		}
	}
}

func (w *World) prepare() {
	w.computeCenters()
	night := w.IsNight()
	for sp := Species(0); sp < SpeciesCount; sp++ {
		v := VisionRadius * w.Tuning.Vision
		f := FleeRadius * w.Genomes[sp].Caution
		if night {
			v *= NightVision
			f *= NightFlee
		}
		w.visionR[sp], w.fleeR[sp] = v, f
		w.restThr[sp] = w.Genomes[sp].restThreshold()
		w.free[sp].Build(w.Agents, sp, false)
	}
}

// mark note ce que fait l'agent suivi par l'interface.
func (w *World) mark(i int, state uint8, target int) {
	if i == w.Focus {
		w.focusState, w.focusTarget = state, target
	}
}

func (w *World) decide(i int) {
	a := &w.Agents[i]
	a.VX, a.VY = 0, 0
	if a.Prisoner {
		w.mark(i, StPrisoner, -1)
		return
	}
	if a.Immune > 0 {
		a.Immune--
	}
	if a.Boost > 0 {
		a.Boost--
	}
	home := &w.Camps[a.Species]
	inHome := home.Contains(a.X, a.Y)

	// Épuisé : immobile jusqu'à reprendre son souffle.
	if a.Exhausted {
		a.recover(inHome)
		if a.Stamina >= StaminaResumeSprint {
			a.Exhausted = false
		}
		w.mark(i, StExhausted, -1)
		return
	}

	if i == w.Player {
		w.playerMove(i, a, inHome)
		return
	}

	// 0. Retour escorté : le prisonnier libéré et son libérateur rentrent au
	// camp, intouchables mais incapables de capturer ou de libérer.
	if a.Immune > 0 {
		dx, dy := w.toward(a, a.Species)
		w.steer(a, dx, dy, a.Speed*EscortFactor)
		w.mark(i, StEscort, -1)
		return
	}

	// 1. Fuite devant les prédateurs proches (sauf à l'abri dans son camp).
	if !inHome {
		if fx, fy, n := w.threat(a); n > 0 {
			w.flee(i, a, home, fx, fy)
			return
		}
	}

	// 2. Récupération : en dessous du seuil, on arrête de courir.
	if !a.Resting && a.Stamina < w.restThr[a.Species] {
		a.Resting = true
	}
	if a.Resting {
		if a.Stamina >= StaminaRestUntil {
			a.Resting = false
		} else {
			w.rest(i, a, home, inHome)
			return
		}
	}

	prey := w.preyOf(a.Species)
	switch a.Role {
	case Liberateur, Leurre:
		if c := w.jailFor(a); c >= 0 {
			if a.Role == Leurre {
				w.decoyMove(i, a, c, inHome)
			} else {
				w.rescueMove(i, a, c, inHome)
			}
			return
		}
	case Gardien:
		w.guardMove(i, a, home, prey, inHome)
		return
	}

	// 3. Chasse de la proie visible la plus proche.
	if j := w.spot(a, prey, w.seeFn); j >= 0 {
		w.chase(i, a, j)
		return
	}

	// 4. Bonus en vue.
	if w.Rules.Bonuses {
		if k, d := w.nearestItem(a); k >= 0 {
			it := &w.Items[k]
			if d < 150 {
				w.sprint(a, it.X-a.X, it.Y-a.Y, ChaseFactor)
			} else {
				w.walk(a, it.X-a.X, it.Y-a.Y, inHome)
			}
			w.mark(i, StItem, -1)
			return
		}
	}

	// 5. Errance vers le centre de gravité des proies.
	w.roam(a, inHome)
	w.mark(i, StRoam, -1)
}

// threat somme les vecteurs d'éloignement des prédateurs libres, non épuisés
// et repérés (hautes herbes) situés à portée de fuite.
func (w *World) threat(a *Agent) (fx, fy float64, n int) {
	g := &w.free[w.predOf(a.Species)]
	r := w.fleeR[a.Species]
	cx0, cy0, cx1, cy1 := g.CellRange(a.X, a.Y, r)
	for cy := cy0; cy <= cy1; cy++ {
		for _, j := range g.RowItems(cy, cx0, cx1) {
			p := &w.Agents[j]
			if p.Exhausted {
				continue
			}
			dx, dy := a.X-p.X, a.Y-p.Y
			d2 := dx*dx + dy*dy
			if d2 > r*r || d2 < 1e-9 {
				continue
			}
			if d2 > GrassSight*GrassSight && w.Terrain[CellIndex(p.X, p.Y)] == TerrGrass {
				continue // embusqué dans les hautes herbes
			}
			d := math.Sqrt(d2)
			fx += dx / d
			fy += dy / d
			n++
		}
	}
	return
}

func (w *World) flee(i int, a *Agent, home *Camp, fx, fy float64) {
	fx, fy = unit(fx, fy)
	hd := hypot(home.X-a.X, home.Y-a.Y)
	if hd < HomeRetreatRadius || a.Stamina < 2*w.restThr[a.Species] || (fx == 0 && fy == 0) {
		hx, hy := unit(w.toward(a, a.Species))
		fx, fy = 0.6*fx+0.4*hx, 0.6*fy+0.4*hy
	}
	if w.Rules.Strength {
		if hx, hy, ok := w.herd(a); ok {
			fx, fy = 0.75*fx+0.25*hx, 0.75*fy+0.25*hy
		}
	}
	if w.Rules.Bonuses {
		if tx, ty, ok := w.tunnelNear(a.X, a.Y, TunnelLure); ok {
			ux, uy := unit(tx-a.X, ty-a.Y)
			if ux*fx+uy*fy > -0.2 {
				fx, fy = ux, uy // plongeon dans le terrier secret
			}
		}
	}
	w.sprint(a, fx, fy, FleeFactor)
	w.mark(i, StFlee, -1)
}

// herd : direction du groupe d'alliés le plus proche (règle « force du nombre »).
func (w *World) herd(a *Agent) (float64, float64, bool) {
	g := &w.free[a.Species]
	cx0, cy0, cx1, cy1 := g.CellRange(a.X, a.Y, HerdRadius)
	var sx, sy float64
	n := 0
	for cy := cy0; cy <= cy1 && n < 24; cy++ {
		for _, j := range g.RowItems(cy, cx0, cx1) {
			b := &w.Agents[j]
			if b == a || b.Exhausted {
				continue
			}
			dx, dy := b.X-a.X, b.Y-a.Y
			if dx*dx+dy*dy > HerdRadius*HerdRadius {
				continue
			}
			sx += dx
			sy += dy
			n++
		}
	}
	if n == 0 {
		return 0, 0, false
	}
	ux, uy := unit(sx, sy)
	return ux, uy, ux != 0 || uy != 0
}

func (w *World) rest(i int, a *Agent, home *Camp, inHome bool) {
	hd := hypot(home.X-a.X, home.Y-a.Y)
	switch {
	case inHome:
		a.WanderAng += (w.rng.Float64() - 0.5) * 0.4
		w.walk(a, math.Cos(a.WanderAng)+(home.X-a.X)/home.R, math.Sin(a.WanderAng)+(home.Y-a.Y)/home.R, true)
	case hd < HomeRetreatRadius:
		dx, dy := w.toward(a, a.Species)
		w.walk(a, dx, dy, false)
	default:
		w.roam(a, false)
	}
	w.mark(i, StRest, -1)
}

// spot : proie la plus proche à portée de vue, repérée et sans obstacle sur
// la ligne de vue ; -1 s'il n'y en a pas.
func (w *World) spot(a *Agent, prey Species, accept func(*Agent) bool) int {
	w.qx, w.qy = a.X, a.Y
	j, _ := w.free[prey].Nearest(w.Agents, a.X, a.Y, w.visionR[a.Species], accept)
	if j >= 0 && !w.lineClear(a.X, a.Y, w.Agents[j].X, w.Agents[j].Y) {
		return -1
	}
	return j
}

// chase sprinte vers la proie j, en visant sa position future (anticipation)
// et en la prenant en tenaille quand un congénère est proche (meute).
func (w *World) chase(i int, a *Agent, j int) {
	b := &w.Agents[j]
	tx, ty := b.X, b.Y
	d := hypot(b.X-a.X, b.Y-a.Y)
	if w.Rules.Lead && d > 1 {
		t := math.Min(d/(a.Speed*ChaseFactor), 25)
		tx += float64(w.prevV[2*j]) * t
		ty += float64(w.prevV[2*j+1]) * t
	}
	if w.Rules.PackHunt && d > 40 && i%3 != 0 {
		w.self = a
		if k, _ := w.free[a.Species].Nearest(w.Agents, a.X, a.Y, PackRadius, w.packFn); k >= 0 {
			// Point de visée décalé sur le flanc de la proie : la moitié de la
			// meute passe à gauche, l'autre à droite.
			sn, cs := flankSin, flankCos
			if i%3 == 2 {
				sn = -sn
			}
			ux, uy := (a.X-b.X)/d, (a.Y-b.Y)/d
			r := math.Min(0.5*d, 120)
			tx += (ux*cs - uy*sn) * r
			ty += (ux*sn + uy*cs) * r
		}
	}
	w.sprint(a, tx-a.X, ty-a.Y, ChaseFactor)
	w.mark(i, StChase, j)
}

// jailFor : camp adverse le plus proche qui détient des coéquipiers, -1 sinon.
func (w *World) jailFor(a *Agent) int {
	best, bestD := -1, math.MaxFloat64
	for c := range w.Camps {
		if Species(c) == a.Species || w.held[c][a.Species] == 0 {
			continue
		}
		camp := &w.Camps[c]
		if d := hypot(camp.X-a.X, camp.Y-a.Y); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func (w *World) rescueMove(i int, a *Agent, c int, inHome bool) {
	jail := &w.Camps[c]
	dx, dy := jail.X-a.X, jail.Y-a.Y
	d := hypot(dx, dy)
	if d > jail.R+30 {
		fx, fy := w.flowDir(&w.campFlow[c], a.X, a.Y, jail.X, jail.Y)
		if d < 700 {
			w.sprint(a, fx, fy, ChaseFactor)
		} else {
			w.walk(a, fx, fy, inHome)
		}
		w.mark(i, StRescue, -1)
		return
	}
	if j, _ := w.jail[a.Species].Nearest(w.Agents, a.X, a.Y, 2*jail.R, w.prisonerFn); j >= 0 {
		b := &w.Agents[j]
		w.sprint(a, b.X-a.X, b.Y-a.Y, ChaseFactor)
		w.mark(i, StRescue, j)
		return
	}
	w.walk(a, dx, dy, inHome)
	w.mark(i, StRescue, -1)
}

// decoyMove : près de la prison, le leurre provoque le gardien le plus proche
// sans entrer dans sa zone de capture ; la fuite l'entraîne ensuite au loin.
func (w *World) decoyMove(i int, a *Agent, c int, inHome bool) {
	jail := &w.Camps[c]
	if hypot(jail.X-a.X, jail.Y-a.Y) < DecoyRange {
		if j, d := w.free[c].Nearest(w.Agents, a.X, a.Y, 1.2*w.visionR[a.Species], w.guardianFn); j >= 0 {
			g := &w.Agents[j]
			dx, dy := g.X-a.X, g.Y-a.Y
			if d > DecoyTaunt {
				w.walk(a, dx, dy, inHome)
			} else {
				w.walk(a, -dy, dx, inHome) // tourne autour du gardien
			}
			w.mark(i, StDecoy, j)
			return
		}
	}
	w.rescueMove(i, a, c, inHome)
}

func (w *World) guardMove(i int, a *Agent, home *Camp, prey Species, inHome bool) {
	if j := w.spot(a, prey, w.guardFn); j >= 0 {
		w.chase(i, a, j)
		return
	}
	// Ronde autour du camp.
	a.WanderAng += 0.004 + (w.rng.Float64()-0.5)*0.01
	tx := home.X + math.Cos(a.WanderAng)*(home.R+120)
	ty := home.Y + math.Sin(a.WanderAng)*(home.R+120)
	w.walk(a, tx-a.X, ty-a.Y, inHome)
	w.mark(i, StGuard, -1)
}

func (w *World) roam(a *Agent, inHome bool) {
	a.WanderAng += (w.rng.Float64() - 0.5) * 0.3
	dx, dy := math.Cos(a.WanderAng), math.Sin(a.WanderAng)
	prey := w.preyOf(a.Species)
	if w.Stats[prey].Free > 0 {
		cx, cy := unit(w.flowDir(&w.preyFlow[prey], a.X, a.Y, w.centerX[prey], w.centerY[prey]))
		dx, dy = 0.6*dx+0.4*cx, 0.6*dy+0.4*cy
	}
	w.walk(a, dx, dy, inHome)
}

// playerMove : l'agent piloté au clavier marche, ou sprinte avec Maj.
func (w *World) playerMove(i int, a *Agent, inHome bool) {
	w.mark(i, StPlayer, -1)
	in := w.Input
	if in.DX == 0 && in.DY == 0 {
		a.recover(inHome)
		return
	}
	if in.Sprint {
		w.sprint(a, float64(in.DX), float64(in.DY), ChaseFactor)
	} else {
		w.walk(a, float64(in.DX), float64(in.DY), inHome)
	}
}

// toward : direction du chemin le plus court vers le camp c.
func (w *World) toward(a *Agent, c Species) (float64, float64) {
	camp := &w.Camps[c]
	return w.flowDir(&w.campFlow[c], a.X, a.Y, camp.X, camp.Y)
}

func (a *Agent) recover(inHome bool) {
	r := StaminaRecoveryWalk
	if inHome {
		r = StaminaRecoveryCamp
	}
	a.Stamina = min(a.Stamina+r, MaxStamina)
}

func (w *World) sprint(a *Agent, dx, dy, factor float64) {
	if a.Boost > 0 {
		w.steer(a, dx, dy, a.Speed*factor*BoostFactor)
		return
	}
	a.Stamina -= StaminaDrainSprint
	if a.Stamina <= 0 {
		a.Stamina = 0
		a.Exhausted = true
		a.Resting = true
		return
	}
	w.steer(a, dx, dy, a.Speed*factor)
}

func (w *World) walk(a *Agent, dx, dy float64, inHome bool) {
	a.recover(inHome)
	w.steer(a, dx, dy, a.Speed*WalkFactor)
}

func (w *World) steer(a *Agent, dx, dy, speed float64) {
	d := hypot(dx, dy)
	if d < 1e-9 {
		dx, dy, d = math.Cos(a.WanderAng), math.Sin(a.WanderAng), 1
	}
	speed *= w.Tuning.Speed
	switch w.Terrain[CellIndex(a.X, a.Y)] {
	case TerrMud:
		speed *= MudFactor
	case TerrGrass:
		speed *= GrassFactor
	}
	a.VX = dx/d*speed + (w.rng.Float64()-0.5)*0.5
	a.VY = dy/d*speed + (w.rng.Float64()-0.5)*0.5
}

func (w *World) move(i int) {
	a := &w.Agents[i]
	w.prevV[2*i], w.prevV[2*i+1] = float32(a.VX), float32(a.VY)
	if a.Prisoner || (a.VX == 0 && a.VY == 0) {
		return
	}
	if w.Blocked(a.X, a.Y) {
		// Pris dans une haie posée pendant la partie : on en sort librement.
		a.X = math.Min(math.Max(a.X+a.VX, WallMargin+1), WorldSize-WallMargin-1)
		a.Y = math.Min(math.Max(a.Y+a.VY, WallMargin+1), WorldSize-WallMargin-1)
		return
	}
	if nx := a.X + a.VX; !w.Blocked(nx, a.Y) {
		a.X = nx
	} else {
		a.WanderAng += math.Pi / 2
	}
	if ny := a.Y + a.VY; !w.Blocked(a.X, ny) {
		a.Y = ny
	} else {
		a.WanderAng += math.Pi / 2
	}
	if len(w.Tunnels) > 0 {
		w.tunnel(a)
	}
}

func (w *World) captures() {
	for i := range w.Agents {
		a := &w.Agents[i]
		if a.Prisoner || a.Exhausted || a.Immune > 0 {
			continue
		}
		j, _ := w.free[w.preyOf(a.Species)].Nearest(w.Agents, a.X, a.Y, CaptureRadius, w.vulnFn)
		if j < 0 {
			continue
		}
		if w.Rules.Strength && w.alliesAround(j) >= StrengthAllies {
			// Le groupe fait front : le prédateur est repoussé et doit souffler.
			a.Stamina = max(a.Stamina-StrengthPenalty, 1)
			a.Resting = true
			continue
		}
		preySp := w.Agents[j].Species
		w.capture(j, a.Species)
		if w.Rules.Frenzy {
			a.Boost = max(a.Boost, FrenzyTicks)
		}
		if i == w.Player {
			w.emit(EvPlayerCapture, int(preySp), -1, 0)
		}
		if j == w.Player {
			w.emit(EvPlayerCaught, int(a.Species), -1, 0)
		}
	}
}

// alliesAround compte les congénères valides à portée d'une proie attaquée.
func (w *World) alliesAround(j int) int {
	b := &w.Agents[j]
	g := &w.free[b.Species]
	cx0, cy0, cx1, cy1 := g.CellRange(b.X, b.Y, StrengthRadius)
	n := 0
	for cy := cy0; cy <= cy1; cy++ {
		for _, k := range g.RowItems(cy, cx0, cx1) {
			c := &w.Agents[k]
			if int(k) == j || c.Prisoner || c.Exhausted {
				continue
			}
			dx, dy := c.X-b.X, c.Y-b.Y
			if dx*dx+dy*dy <= StrengthRadius*StrengthRadius {
				n++
			}
		}
	}
	return n
}

// capture envoie l'agent j dans la prison du camp de l'espèce by.
// Les places suivent une spirale de tournesol depuis le centre du camp.
func (w *World) capture(j int, by Species) {
	b := &w.Agents[j]
	w.addHeat(0, b.X, b.Y)
	camp := &w.Camps[by]
	slot := w.jailSeq[by]
	w.jailSeq[by]++
	k := float64(slot % int32(w.PerSpecies))
	r := camp.R * 0.9 * math.Sqrt((k+0.5)/float64(w.PerSpecies))
	theta := k * 2.399963229728653 // angle d'or
	*b = Agent{
		X:         camp.X + r*math.Cos(theta),
		Y:         camp.Y + r*math.Sin(theta),
		Speed:     b.Speed,
		WanderAng: b.WanderAng,
		Stamina:   b.Stamina,
		Species:   b.Species,
		Role:      b.Role,
		Jail:      by,
		Prisoner:  true,
	}
	w.Stats[by].Captures++
}

// release rend la liberté à un captif, qui rentre au camp en retour escorté.
func (w *World) release(b *Agent) {
	b.Prisoner = false
	b.Immune = ImmunityTicks
	b.Stamina = MaxStamina
	b.JailTicks = 0
}

// rescues : un agent libre au contact d'un coéquipier prisonnier en délivre
// un seul ; les deux repartent en retour escorté (immunisés, sans pouvoir
// capturer ni libérer). Un agent en retour escorté ne libère personne : pas de
// libération en chaîne qui viderait une prison en quelques ticks.
func (w *World) rescues() {
	for i := range w.Agents {
		a := &w.Agents[i]
		if a.Prisoner || a.Exhausted || a.Immune > 0 {
			continue
		}
		for c := range w.Camps {
			if Species(c) == a.Species {
				continue
			}
			jail := &w.Camps[c]
			dx, dy := a.X-jail.X, a.Y-jail.Y
			reach := jail.R + RescueRadius
			if dx*dx+dy*dy > reach*reach {
				continue
			}
			if j, _ := w.jail[a.Species].Nearest(w.Agents, a.X, a.Y, RescueRadius, w.prisonerFn); j >= 0 {
				w.release(&w.Agents[j])
				a.Immune = ImmunityTicks
				a.Resting = false
				w.Stats[a.Species].Rescues++
				w.addHeat(1, a.X, a.Y)
				if i == w.Player {
					w.emit(EvPlayerRescue, int(a.Species), -1, 0)
				}
				if j == w.Player {
					w.emit(EvPlayerFreed, int(a.Species), -1, 0)
				}
			}
			break
		}
	}
}

func (w *World) refreshStats() {
	for sp := range w.Stats {
		s := &w.Stats[sp]
		s.Free, s.Prisoners, s.Exhausted = 0, 0, 0
	}
	w.held = [SpeciesCount][SpeciesCount]int{}
	for i := range w.Agents {
		a := &w.Agents[i]
		s := &w.Stats[a.Species]
		if a.Prisoner {
			s.Prisoners++
			w.held[a.Jail][a.Species]++
			continue
		}
		s.Free++
		if a.Exhausted {
			s.Exhausted++
		}
	}
}

// checkEnd : une espèce dont tous les membres sont prisonniers ne peut plus
// délivrer personne. La partie s'arrête quand une seule espèce reste libre, ou
// au temps limite (victoire de l'espèce la plus nombreuse en liberté).
func (w *World) checkEnd() {
	alive, last := 0, -1
	for sp := range w.Stats {
		if w.Stats[sp].Free > 0 {
			alive++
			last = sp
			w.eliminated[sp] = false
		} else if !w.eliminated[sp] {
			w.eliminated[sp] = true
			w.emit(EvEliminated, sp, -1, 0)
		}
	}
	if alive <= 1 {
		w.finish(last)
		return
	}
	if w.Tick >= w.MaxTicks {
		winner, best := -1, 0
		for sp := range w.Stats {
			switch f := w.Stats[sp].Free; {
			case f > best:
				best, winner = f, sp
			case f == best:
				winner = -1
			}
		}
		w.finish(winner)
		return
	}
	if w.Tick%30 == 0 && w.Tick >= 300 {
		w.leaderCheck()
	}
}

func (w *World) finish(winner int) {
	w.Finished, w.Winner = true, winner
	w.emit(EvEnd, winner, -1, 0)
}

// leaderCheck annonce un changement d'espèce en tête, avec une marge pour
// éviter d'annoncer chaque dépassement d'une courte tête.
func (w *World) leaderCheck() {
	best := 0
	for sp := 1; sp < SpeciesCount; sp++ {
		if w.Stats[sp].Free > w.Stats[best].Free {
			best = sp
		}
	}
	switch {
	case w.leader < 0:
		w.leader = best
	case best != w.leader && w.Stats[best].Free >= w.Stats[w.leader].Free+60:
		w.leader = best
		w.emit(EvLeader, best, -1, 0)
	}
}

// hypot sans les précautions de math.Hypot contre les débordements, inutiles
// à l'échelle de l'arène et coûteuses dans les boucles par agent.
func hypot(x, y float64) float64 { return math.Sqrt(x*x + y*y) }

func unit(x, y float64) (float64, float64) {
	d := hypot(x, y)
	if d < 1e-9 {
		return 0, 0
	}
	return x / d, y / d
}

// ---------------------------------------------------------------------------
// Lancement
// ---------------------------------------------------------------------------

//go:embed index.html
var indexHTML []byte

// serverDefaults : configuration des parties ouvertes depuis le navigateur.
var serverDefaults = DefaultConfig(MasterSeed)

func main() {
	tournoi := flag.Int("tournoi", 0, "joue N parties sans interface, en parallèle, et affiche le bilan")
	seed := flag.Int64("seed", MasterSeed, "graine de la première partie")
	regles := flag.String("regles", "toutes", "règles actives : toutes, aucune, ou liste ("+RuleList()+")")
	individus := flag.Int("individus", DefaultPerSpecies, fmt.Sprintf("individus par espèce (%d à %d)", MinPerSpecies, MaxPerSpecies))
	duree := flag.Float64("duree", float64(DefaultMinutes), fmt.Sprintf("durée d'une manche en minutes (0,5 à %d)", MaxMinutes))
	port := flag.Int("port", Port, "port HTTP du serveur")
	flag.Parse()

	rules, err := ParseRules(*regles)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	serverDefaults.Seed = *seed
	serverDefaults.Rules = rules
	serverDefaults.PerSpecies = *individus
	serverDefaults.MaxTicks = MinutesToTicks(*duree)
	serverDefaults = serverDefaults.Clamp()
	if *tournoi > 0 {
		runTournamentCLI(serverDefaults, *tournoi)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write(indexHTML)
	})
	mux.Handle("/ws", wsHandler())

	fmt.Printf("Arène Poule · Renard · Vipère sur http://localhost:%d (seed %d, 3 × %d agents, %s par manche, règles : %s)\n",
		*port, *seed, serverDefaults.PerSpecies, clockText(serverDefaults.MaxTicks), rules)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("localhost:%d", *port), mux))
}
