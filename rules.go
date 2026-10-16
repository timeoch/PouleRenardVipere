package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
)

// ---------------------------------------------------------------------------
// Paramètres des règles optionnelles
// ---------------------------------------------------------------------------

const (
	NightPeriod   = 1800 // une nuit par minute simulée
	NightDuration = 360  // qui dure 12 s
	NightVision   = 0.45 // la vue tombe à 45 % la nuit
	NightFlee     = 0.65 // et l'on repère les prédateurs plus tard

	EscapeGuardRadius = 450.0 // gardiens comptés à cette distance du centre du camp
	PrisonersPerGuard = 3     // un gardien éveillé tient 3 captifs
	EscapePerTick     = 0.08  // probabilité d'évasion par tick d'une prison sans gardien (~2,4 /s)

	StrengthRadius        = 45.0 // alliés comptés autour d'une proie attaquée
	StrengthAllies        = 2    // à partir de 2 alliés, la capture échoue
	StrengthPenalty int16 = 15   // endurance perdue par le prédateur repoussé
	HerdRadius            = 60.0 // en fuite, on se rapproche des alliés à cette distance

	FrenzyTicks uint8 = 60  // 2 s de sprint gratuit après une capture
	PlumeTicks  uint8 = 150 // 5 s de sprint gratuit avec une plume

	StockholmTicks uint16 = 1800 // 60 s de détention avant de rejoindre ses geôliers
	ConvertReport         = 150  // passages annoncés en cumul toutes les 5 s

	MaxItems     = 6
	ItemEvery    = 150 // un bonus toutes les 5 s
	EggChance    = 0.06
	PickRadius   = 18.0
	ItemSight    = 350.0
	TunnelPairs  = 2
	TunnelRadius = 22.0
	TunnelLure   = 160.0 // un fuyard plonge dans un terrier aussi proche

	GrassSight      = 70.0 // dans les hautes herbes, on n'est repéré que de près
	MudFactor       = 0.55
	GrassFactor     = 0.85
	RiverHalfWidth  = 70.0
	BridgeHalfWidth = 110.0
	LakeRadius      = 230.0

	RoleEvery = 30 // rôles recalculés chaque seconde
	FlowEvery = 15 // chemins vers les troupeaux recalculés deux fois par seconde
	HeatCols  = 50
	HeatCell  = WorldSize / HeatCols
	EventRing = 256
)

var flankSin, flankCos = math.Sincos(0.9) // angle de prise en tenaille

// ---------------------------------------------------------------------------
// Configuration d'une partie
// ---------------------------------------------------------------------------

// Rules : interrupteurs des règles optionnelles, fixés au lancement. Une même
// graine avec les mêmes règles rejoue exactement la même partie.
type Rules struct {
	Night        bool `json:"night"`
	Escape       bool `json:"escape"`
	Strength     bool `json:"strength"`
	Frenzy       bool `json:"frenzy"`
	Stockholm    bool `json:"stockholm"`
	Reversal     bool `json:"reversal"`
	Terrain      bool `json:"terrain"`
	Bonuses      bool `json:"bonuses"`
	PackHunt     bool `json:"pack"`
	Lead         bool `json:"lead"`
	Diversion    bool `json:"diversion"`
	DynamicRoles bool `json:"dynamic"`
}

// RuleKeys suit l'ordre de Rules.fields et les étiquettes JSON.
var RuleKeys = [...]string{"night", "escape", "strength", "frenzy", "stockholm", "reversal",
	"terrain", "bonuses", "pack", "lead", "diversion", "dynamic"}

func (r *Rules) fields() [len(RuleKeys)]*bool {
	return [...]*bool{&r.Night, &r.Escape, &r.Strength, &r.Frenzy, &r.Stockholm, &r.Reversal,
		&r.Terrain, &r.Bonuses, &r.PackHunt, &r.Lead, &r.Diversion, &r.DynamicRoles}
}

func AllRules() Rules {
	var r Rules
	for _, p := range r.fields() {
		*p = true
	}
	return r
}

func RuleList() string { return strings.Join(RuleKeys[:], ",") }

// ParseRules lit « toutes », « aucune » ou une liste de clés séparées par des virgules.
func ParseRules(s string) (Rules, error) {
	switch strings.TrimSpace(s) {
	case "", "toutes", "all":
		return AllRules(), nil
	case "aucune", "none":
		return Rules{}, nil
	}
	var r Rules
	f := r.fields()
next:
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		for i, name := range RuleKeys {
			if k == name {
				*f[i] = true
				continue next
			}
		}
		return r, fmt.Errorf("règle inconnue %q (règles : %s)", k, RuleList())
	}
	return r, nil
}

func (r Rules) String() string {
	var on []string
	for i, p := range r.fields() {
		if *p {
			on = append(on, RuleKeys[i])
		}
	}
	switch len(on) {
	case 0:
		return "aucune"
	case len(RuleKeys):
		return "toutes"
	}
	return strings.Join(on, ",")
}

// Tuning : réglages globaux modifiables en direct.
type Tuning struct {
	Vision float64 `json:"vision"` // multiplicateur du rayon de vision
	Speed  float64 `json:"speed"`  // multiplicateur de vitesse
}

// Genome : stratégie d'une espèce, réglable en direct et soumise à l'évolution.
type Genome struct {
	Guards   float64 `json:"guards"`   // part de gardiens
	Rescuers float64 `json:"rescuers"` // part de libérateurs (leurres compris)
	Caution  float64 `json:"caution"`  // multiplicateur du rayon de fuite
	Grit     float64 `json:"grit"`     // ténacité : 1 = chasse jusqu'au bout du souffle
}

var DefaultGenome = Genome{Guards: GuardRatio, Rescuers: RescuerRatio, Caution: 1, Grit: 0.5}
var DefaultTuning = Tuning{Vision: 1, Speed: 1}

func clampF(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return lo
	}
	return math.Min(math.Max(v, lo), hi)
}

func (t Tuning) Clamp() Tuning {
	return Tuning{Vision: clampF(t.Vision, 0.5, 2), Speed: clampF(t.Speed, 0.5, 2)}
}

func (g Genome) Clamp() Genome {
	g.Guards = clampF(g.Guards, 0.01, 0.35)
	g.Rescuers = clampF(g.Rescuers, 0.02, 0.45)
	g.Caution = clampF(g.Caution, 0.4, 1.8)
	g.Grit = clampF(g.Grit, 0, 1)
	return g
}

// restThreshold : endurance sous laquelle l'agent arrête de chasser (25 par défaut).
func (g Genome) restThreshold() int16 {
	return int16(math.Round(float64(StaminaLowThreshold) * (1.8 - 1.6*g.Grit)))
}

type Config struct {
	Seed       int64
	Rules      Rules
	PerSpecies int // individus par espèce
	MaxTicks   int // durée de la manche en ticks
	Tuning     Tuning
	Genomes    [SpeciesCount]Genome
	Workers    int // goroutines de la phase parallèle (0 : DefaultWorkers)
}

func DefaultConfig(seed int64) Config {
	return Config{Seed: seed, Rules: AllRules(), PerSpecies: DefaultPerSpecies,
		MaxTicks: DefaultMinutes * 60 * TargetTPS, Tuning: DefaultTuning,
		Genomes: [SpeciesCount]Genome{DefaultGenome, DefaultGenome, DefaultGenome}}
}

// Clamp borne l'effectif (MinPerSpecies…MaxPerSpecies) et la durée (de 30 s
// à MaxMinutes) ; une valeur nulle prend la valeur par défaut.
func (c Config) Clamp() Config {
	if c.PerSpecies == 0 {
		c.PerSpecies = DefaultPerSpecies
	}
	if c.MaxTicks == 0 {
		c.MaxTicks = DefaultMinutes * 60 * TargetTPS
	}
	c.PerSpecies = min(max(c.PerSpecies, MinPerSpecies), MaxPerSpecies)
	c.MaxTicks = min(max(c.MaxTicks, 30*TargetTPS), MaxMinutes*60*TargetTPS)
	return c
}

// MinutesToTicks convertit une durée en minutes (décimales permises) en ticks.
func MinutesToTicks(m float64) int { return int(math.Round(m * 60 * float64(TargetTPS))) }

// ReversalTick : la chaîne s'inverse à mi-manche.
func (w *World) ReversalTick() int { return w.MaxTicks / 2 }

// ClassicConfig : le jeu d'origine, sans aucune règle optionnelle.
func ClassicConfig(seed int64) Config {
	c := DefaultConfig(seed)
	c.Rules = Rules{}
	return c
}

// ---------------------------------------------------------------------------
// Terrain : rivière en Y avec ponts, boue, hautes herbes
// ---------------------------------------------------------------------------

const (
	TerrPlain uint8 = iota
	TerrMud
	TerrGrass
	TerrWater
	TerrBridge
)

// genTerrain trace trois bras de rivière depuis un lac central, chacun dans
// le prolongement d'un camp : chaque paire de camps est séparée par un bras
// franchissable sur deux ponts. Le terrain a son propre générateur.
func (w *World) genTerrain() {
	rng := rand.New(rand.NewPCG(uint64(w.Seed), 0x7e44a1))
	var gx, gy float64
	for c := range w.Camps {
		gx += w.Camps[c].X / SpeciesCount
		gy += w.Camps[c].Y / SpeciesCount
	}
	var bridges [][2]float64
	for b := range w.Camps {
		dx, dy := unit(gx-w.Camps[b].X, gy-w.Camps[b].Y)
		px, py := -dy, dx
		phase := rng.Float64() * 2 * math.Pi
		freq := 0.004 + rng.Float64()*0.003
		amp := 60 + rng.Float64()*80
		length := rayToWall(gx, gy, dx, dy)
		at := func(t float64) (float64, float64) {
			wob := amp * math.Sin(t*freq+phase) * math.Min(1, t/400) // le bras part droit du lac
			return gx + dx*t + px*wob, gy + dy*t + py*wob
		}
		for t := 0.0; t <= length+RiverHalfWidth; t += 15 {
			x, y := at(t)
			w.paintDisk(x, y, RiverHalfWidth, TerrWater, TerrPlain)
		}
		for _, f := range [2]float64{0.35, 0.75} {
			x, y := at((f + (rng.Float64()-0.5)*0.1) * length)
			bridges = append(bridges, [2]float64{x, y})
		}
	}
	w.paintDisk(gx, gy, LakeRadius, TerrWater, TerrPlain)
	for _, p := range bridges {
		w.paintDisk(p[0], p[1], BridgeHalfWidth, TerrBridge, TerrWater)
	}

	blob := func(kind uint8, rMin, rMax float64) {
		for try := 0; try < 20; try++ {
			x := 300 + rng.Float64()*(WorldSize-600)
			y := 300 + rng.Float64()*(WorldSize-600)
			r := rMin + rng.Float64()*(rMax-rMin)
			if w.nearCamp(x, y, r+150) {
				continue
			}
			w.paintDisk(x, y, r, kind, TerrPlain)
			return
		}
	}
	for k := 0; k < 9; k++ {
		blob(TerrMud, 140, 320)
	}
	for k := 0; k < 12; k++ {
		blob(TerrGrass, 160, 380)
	}
}

// rayToWall : distance du point (x, y) au mur dans la direction (dx, dy).
func rayToWall(x, y, dx, dy float64) float64 {
	t := math.Inf(1)
	if dx > 1e-9 {
		t = math.Min(t, (WorldSize-x)/dx)
	} else if dx < -1e-9 {
		t = math.Min(t, -x/dx)
	}
	if dy > 1e-9 {
		t = math.Min(t, (WorldSize-y)/dy)
	} else if dy < -1e-9 {
		t = math.Min(t, -y/dy)
	}
	return t
}

// paintDisk peint en kind les cellules de type over dont le centre est dans le disque.
func (w *World) paintDisk(x, y, r float64, kind, over uint8) {
	for cy := cellCoord(y-r, GridRows); cy <= cellCoord(y+r, GridRows); cy++ {
		for cx := cellCoord(x-r, GridCols); cx <= cellCoord(x+r, GridCols); cx++ {
			ccx, ccy := (float64(cx)+0.5)*CellSize, (float64(cy)+0.5)*CellSize
			c := cy*GridCols + cx
			if (ccx-x)*(ccx-x)+(ccy-y)*(ccy-y) <= r*r && w.Terrain[c] == over {
				w.Terrain[c] = kind
			}
		}
	}
}

func (w *World) nearCamp(x, y, margin float64) bool {
	for c := range w.Camps {
		camp := &w.Camps[c]
		if hypot(camp.X-x, camp.Y-y) < camp.R+margin {
			return true
		}
	}
	return false
}

func (w *World) rectNearCamp(ox, oy, ow, oh, margin float64) bool {
	for c := range w.Camps {
		camp := &w.Camps[c]
		nx := math.Max(ox, math.Min(camp.X, ox+ow))
		ny := math.Max(oy, math.Min(camp.Y, oy+oh))
		if hypot(camp.X-nx, camp.Y-ny) < camp.R+margin {
			return true
		}
	}
	return false
}

// rectTouchesWater : le rectangle (élargi d'une cellule) touche l'eau ou un
// pont. Une haie ne doit jamais boucher un pont.
func (w *World) rectTouchesWater(ox, oy, ow, oh float64) bool {
	for cy := cellCoord(oy-CellSize, GridRows); cy <= cellCoord(oy+oh+CellSize, GridRows); cy++ {
		for cx := cellCoord(ox-CellSize, GridCols); cx <= cellCoord(ox+ow+CellSize, GridCols); cx++ {
			if t := w.Terrain[cy*GridCols+cx]; t == TerrWater || t == TerrBridge {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Chemins : champs de distance par parcours en largeur sur la grille
// ---------------------------------------------------------------------------

const (
	flowInf  = math.MaxUint16
	flowNone = math.MaxUint8
)

// FlowField : nombre de cellules à parcourir jusqu'à la cible (flowInf :
// inaccessible) et, pour chaque cellule, le voisin à rejoindre (index dans
// nbX/nbY, flowNone pour la cible elle-même).
type FlowField struct {
	Dist [GridCells]uint16
	Next [GridCells]uint8
}

var nbX = [8]int{0, 0, -1, 1, -1, 1, -1, 1}
var nbY = [8]int{-1, 1, 0, 0, -1, -1, 1, 1}
var nbLen = [8]float64{1, 1, 1, 1, math.Sqrt2, math.Sqrt2, math.Sqrt2, math.Sqrt2}

// computeFreeCells marque les cellules entièrement praticables (9 points testés).
func (w *World) computeFreeCells() {
	for cy := 0; cy < GridRows; cy++ {
		for cx := 0; cx < GridCols; cx++ {
			free := true
			for s := 0; s < 9 && free; s++ {
				x := (float64(cx) + float64(2*(s%3)+1)/6) * CellSize
				y := (float64(cy) + float64(2*(s/3)+1)/6) * CellSize
				free = !w.Blocked(x, y)
			}
			w.cellFree[cy*GridCols+cx] = free
		}
	}
}

func (w *World) diagonalOK(cx, cy, nx, ny int) bool {
	return w.cellFree[cy*GridCols+nx] && w.cellFree[ny*GridCols+cx]
}

// buildFlow calcule le champ vers (tx, ty), sans allocation : distances par
// parcours en largeur, puis le prochain pas de chaque cellule. Parmi les
// voisins qui rapprochent le plus, on prend le mieux aligné avec la ligne
// droite vers la cible : trajet direct en terrain dégagé, contournement des
// haies et passage par les ponts sinon. Les cellules bloquées pointent vers
// le voisin praticable le plus proche du but, pour en sortir.
func (w *World) buildFlow(f *FlowField, tx, ty float64) {
	d := &f.Dist
	for i := range d {
		d[i] = flowInf
		f.Next[i] = flowNone
	}
	start := w.nearestFreeCell(CellIndex(tx, ty))
	if start < 0 {
		return
	}
	q := w.flowQueue[:0]
	if naiveAlloc {
		q = nil
	}
	d[start] = 0
	q = append(q, int32(start))
	for h := 0; h < len(q); h++ {
		c := int(q[h])
		cx, cy := c%GridCols, c/GridCols
		next := d[c] + 1
		for k := 0; k < 8; k++ {
			nx, ny := cx+nbX[k], cy+nbY[k]
			if nx < 0 || ny < 0 || nx >= GridCols || ny >= GridRows {
				continue
			}
			n := ny*GridCols + nx
			if d[n] != flowInf || !w.cellFree[n] || (k >= 4 && !w.diagonalOK(cx, cy, nx, ny)) {
				continue
			}
			d[n] = next
			q = append(q, int32(n))
		}
	}

	for c := range d {
		cur := d[c]
		if cur == 0 {
			continue
		}
		cx, cy := c%GridCols, c/GridCols
		ddx := tx - (float64(cx)+0.5)*CellSize
		ddy := ty - (float64(cy)+0.5)*CellSize
		dl := hypot(ddx, ddy) + 1e-9
		best := uint16(flowInf)
		bestAlign := -2.0
		for k := 0; k < 8; k++ {
			nx, ny := cx+nbX[k], cy+nbY[k]
			if nx < 0 || ny < 0 || nx >= GridCols || ny >= GridRows {
				continue
			}
			v := d[ny*GridCols+nx]
			if v >= cur || v > best || (k >= 4 && !w.diagonalOK(cx, cy, nx, ny)) {
				continue
			}
			align := (float64(nbX[k])*ddx + float64(nbY[k])*ddy) / (nbLen[k] * dl)
			if v < best || align > bestAlign {
				best, bestAlign = v, align
				f.Next[c] = uint8(k)
			}
		}
	}
}

func (w *World) nearestFreeCell(c int) int {
	if w.cellFree[c] {
		return c
	}
	cx, cy := c%GridCols, c/GridCols
	for r := 1; r <= 8; r++ {
		for dy := -r; dy <= r; dy++ {
			for dx := -r; dx <= r; dx++ {
				nx, ny := cx+dx, cy+dy
				if max(abs(dx), abs(dy)) != r || nx < 0 || ny < 0 || nx >= GridCols || ny >= GridRows {
					continue
				}
				if n := ny*GridCols + nx; w.cellFree[n] {
					return n
				}
			}
		}
	}
	return -1
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// flowDir : direction à suivre depuis (x, y) pour rejoindre la cible du champ
// f, soit le centre de la cellule voisine précalculée, soit (tx, ty) tout
// droit quand on est arrivé.
func (w *World) flowDir(f *FlowField, x, y, tx, ty float64) (float64, float64) {
	c := CellIndex(x, y)
	k := f.Next[c]
	if f.Dist[c] <= 1 || k == flowNone {
		return tx - x, ty - y
	}
	nx, ny := c%GridCols+nbX[k], c/GridCols+nbY[k]
	return (float64(nx)+0.5)*CellSize - x, (float64(ny)+0.5)*CellSize - y
}

func (w *World) buildCampFlows() {
	for c := range w.Camps {
		w.buildFlow(&w.campFlow[c], w.Camps[c].X, w.Camps[c].Y)
	}
}

// buildPreyFlows recalcule les chemins vers le centre de gravité de chaque
// espèce. Il ne dépend que de l'état en fin de tick, ce qui permet au replay
// de le reconstruire à l'identique.
func (w *World) buildPreyFlows() {
	w.computeCenters()
	for sp := range w.preyFlow {
		w.buildFlow(&w.preyFlow[sp], w.centerX[sp], w.centerY[sp])
	}
}

// ---------------------------------------------------------------------------
// Nuit et renversement de la chaîne
// ---------------------------------------------------------------------------

func (w *World) IsNight() bool {
	return w.Rules.Night && w.Tick%NightPeriod >= NightPeriod-NightDuration
}

func (w *World) seasons() {
	if w.Rules.Night {
		switch w.Tick % NightPeriod {
		case NightPeriod - NightDuration:
			w.emit(EvNightFall, -1, -1, 0)
		case 0:
			w.emit(EvDawn, -1, -1, 0)
		}
	}
	if w.Rules.Reversal {
		switch w.Tick {
		case w.ReversalTick() - 300:
			w.emit(EvReversalSoon, -1, -1, 0)
		case w.ReversalTick():
			w.Reversed = !w.Reversed
			w.emit(EvReversal, -1, -1, 0)
		}
	}
}

// ---------------------------------------------------------------------------
// Bonus au sol et terriers secrets
// ---------------------------------------------------------------------------

type ItemKind uint8

const (
	Plume ItemKind = iota // sprint gratuit 5 s
	Oeuf                  // libère tous les captifs de l'espèce qui le ramasse
)

type Item struct {
	X      float64  `json:"x"`
	Y      float64  `json:"y"`
	Kind   ItemKind `json:"k"`
	Active bool     `json:"-"`
}

// Tunnel relie deux entrées : un agent qui entre par l'une ressort par l'autre.
type Tunnel struct {
	X [2]float64 `json:"x"`
	Y [2]float64 `json:"y"`
}

func (w *World) genTunnels() {
	rng := rand.New(rand.NewPCG(uint64(w.Seed), 0x7e44a2))
	spot := func() (float64, float64) {
		for try := 0; ; try++ {
			x := 300 + rng.Float64()*(WorldSize-600)
			y := 300 + rng.Float64()*(WorldSize-600)
			if try > 500 || (!w.Blocked(x, y) && !w.nearCamp(x, y, 450)) {
				return x, y
			}
		}
	}
	for k := 0; k < TunnelPairs; k++ {
		var t Tunnel
		t.X[0], t.Y[0] = spot()
		for try := 0; try < 200; try++ {
			t.X[1], t.Y[1] = spot()
			if hypot(t.X[1]-t.X[0], t.Y[1]-t.Y[0]) > 1800 {
				break
			}
		}
		w.Tunnels = append(w.Tunnels, t)
	}
}

// tunnel téléporte un agent entré dans un terrier vers l'autre entrée, en le
// déposant juste à côté dans son sens de marche (pas d'aller-retour).
func (w *World) tunnel(a *Agent) {
	for k := range w.Tunnels {
		t := &w.Tunnels[k]
		for e := 0; e < 2; e++ {
			dx, dy := a.X-t.X[e], a.Y-t.Y[e]
			if dx*dx+dy*dy > TunnelRadius*TunnelRadius {
				continue
			}
			ux, uy := unit(a.VX, a.VY)
			for try := 0; try < 4; try++ {
				nx, ny := t.X[1-e]+ux*(TunnelRadius+8), t.Y[1-e]+uy*(TunnelRadius+8)
				if !w.Blocked(nx, ny) {
					a.X, a.Y = nx, ny
					return
				}
				ux, uy = -uy, ux
			}
			return
		}
	}
}

func (w *World) tunnelNear(x, y, r float64) (float64, float64, bool) {
	bx, by, best := 0.0, 0.0, r*r
	found := false
	for k := range w.Tunnels {
		t := &w.Tunnels[k]
		for e := 0; e < 2; e++ {
			if d2 := (t.X[e]-x)*(t.X[e]-x) + (t.Y[e]-y)*(t.Y[e]-y); d2 < best {
				bx, by, best, found = t.X[e], t.Y[e], d2, true
			}
		}
	}
	return bx, by, found
}

func (w *World) nearestItem(a *Agent) (int, float64) {
	best, bestD := -1, ItemSight
	for k := range w.Items {
		it := &w.Items[k]
		if !it.Active {
			continue
		}
		if d := hypot(it.X-a.X, it.Y-a.Y); d < bestD && w.lineClear(a.X, a.Y, it.X, it.Y) {
			best, bestD = k, d
		}
	}
	return best, bestD
}

func (w *World) spawnItem() {
	slot := -1
	for k := range w.Items {
		if !w.Items[k].Active {
			slot = k
			break
		}
	}
	if slot < 0 {
		return
	}
	for try := 0; try < 30; try++ {
		x := 250 + w.rng.Float64()*(WorldSize-500)
		y := 250 + w.rng.Float64()*(WorldSize-500)
		if w.Blocked(x, y) || w.nearCamp(x, y, 200) {
			continue
		}
		kind := Plume
		if w.rng.Float64() < EggChance {
			kind = Oeuf
		}
		w.Items[slot] = Item{X: x, Y: y, Kind: kind, Active: true}
		return
	}
}

// pickups : l'agent libre et éveillé le plus proche d'un bonus le ramasse.
func (w *World) pickups() {
	for k := range w.Items {
		it := &w.Items[k]
		if !it.Active {
			continue
		}
		best, bestD := -1, 0.0
		for sp := range w.free {
			j, d := w.free[sp].Nearest(w.Agents, it.X, it.Y, PickRadius, w.awakeFn)
			if j >= 0 && (best < 0 || d < bestD || (d == bestD && j < best)) {
				best, bestD = j, d
			}
		}
		if best < 0 {
			continue
		}
		it.Active = false
		a := &w.Agents[best]
		switch it.Kind {
		case Plume:
			a.Boost = max(a.Boost, PlumeTicks)
			w.emit(EvPlume, int(a.Species), -1, 0)
		case Oeuf:
			n := 0
			for i := range w.Agents {
				if b := &w.Agents[i]; b.Prisoner && b.Species == a.Species {
					w.release(b)
					n++
				}
			}
			w.Stats[a.Species].Rescues += n
			w.emit(EvEgg, int(a.Species), -1, n)
		}
	}
}

// ---------------------------------------------------------------------------
// Évasions et syndrome de Stockholm
// ---------------------------------------------------------------------------

// guardsNear compte les gardiens éveillés aux abords du camp c.
func (w *World) guardsNear(c Species) int {
	g := &w.free[c]
	camp := &w.Camps[c]
	cx0, cy0, cx1, cy1 := g.CellRange(camp.X, camp.Y, EscapeGuardRadius)
	n := 0
	for cy := cy0; cy <= cy1; cy++ {
		for _, j := range g.RowItems(cy, cx0, cx1) {
			b := &w.Agents[j]
			dx, dy := b.X-camp.X, b.Y-camp.Y
			if w.guardianFn(b) && dx*dx+dy*dy <= EscapeGuardRadius*EscapeGuardRadius {
				n++
			}
		}
	}
	return n
}

// escapes : une prison sous-gardée (moins d'un gardien éveillé pour 3
// captifs à ses abords) laisse filer des captifs, d'autant plus vite que les
// gardiens manquent. Les grandes prisons deviennent coûteuses à tenir.
func (w *World) escapes() {
	for c := Species(0); c < SpeciesCount; c++ {
		n := w.held[c][0] + w.held[c][1] + w.held[c][2]
		if n == 0 {
			w.unguarded[c] = false
			continue
		}
		need := (n + PrisonersPerGuard - 1) / PrisonersPerGuard
		guards := w.guardsNear(c)
		if guards >= need {
			w.unguarded[c] = false
			continue
		}
		weak := 2*guards < need
		if weak && !w.unguarded[c] && w.Tick-w.unguardAt[c] > 300 {
			w.emit(EvUnguarded, int(c), guards, n)
			w.unguardAt[c] = w.Tick
		}
		w.unguarded[c] = weak
		if w.rng.Float64() >= EscapePerTick*(1-float64(guards)/float64(need)) {
			continue
		}
		count := 0
		for i := range w.Agents {
			if b := &w.Agents[i]; b.Prisoner && b.Jail == c {
				count++
			}
		}
		if count == 0 {
			continue
		}
		k := w.rng.IntN(count)
		for i := range w.Agents {
			b := &w.Agents[i]
			if !b.Prisoner || b.Jail != c {
				continue
			}
			if k == 0 {
				w.release(b)
				w.Stats[b.Species].Escapes++
				if i == w.Player {
					w.emit(EvPlayerFreed, int(b.Species), -1, 0)
				}
				break
			}
			k--
		}
	}
}

// prisonTime compte le temps de détention ; avec la règle « Stockholm », un
// captif détenu trop longtemps rejoint l'espèce de ses geôliers. Les passages
// sont annoncés en cumul toutes les 5 s plutôt qu'à chaque tick.
func (w *World) prisonTime() {
	conv := &w.convAcc
	for i := range w.Agents {
		a := &w.Agents[i]
		if !a.Prisoner {
			continue
		}
		if a.JailTicks < math.MaxUint16 {
			a.JailTicks++
		}
		if !w.Rules.Stockholm || a.JailTicks < StockholmTicks {
			continue
		}
		from, to := a.Species, a.Jail
		a.Species = to
		a.Prisoner, a.Exhausted, a.Resting = false, false, false
		a.JailTicks = 0
		a.Stamina = MaxStamina
		g, r := w.roleBands(to)
		a.Role = pickRole(roleRoll(w.Seed, i), g, r, w.Rules.Diversion)
		w.Stats[to].Recruits++
		conv[from][to]++
	}
	if w.Tick%ConvertReport != 0 {
		return
	}
	for from := range conv {
		for to, n := range conv[from] {
			if n > 0 {
				w.emit(EvConvert, from, to, n)
				conv[from][to] = 0
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Rôles
// ---------------------------------------------------------------------------

// roleBands : parts de gardiens et de libérateurs de l'espèce. Avec les rôles
// dynamiques, les libérateurs se multiplient quand beaucoup de coéquipiers
// sont captifs, et les gardiens quand la prison du camp se remplit.
func (w *World) roleBands(sp Species) (guards, rescuers float64) {
	g := &w.Genomes[sp]
	guards, rescuers = g.Guards, g.Rescuers
	if w.Rules.DynamicRoles {
		st := &w.Stats[sp]
		if m := st.Free + st.Prisoners; m > 0 {
			held := w.held[sp][0] + w.held[sp][1] + w.held[sp][2]
			rescuers += 0.5 * float64(st.Prisoners) / float64(m)
			guards += 0.3 * math.Min(1, float64(held)/float64(m))
		}
		if s := guards + rescuers; s > 0.9 {
			guards, rescuers = guards*0.9/s, rescuers*0.9/s
		}
	}
	return
}

func pickRole(roll, guards, rescuers float64, diversion bool) Role {
	switch {
	case roll < guards:
		return Gardien
	case roll < guards+rescuers:
		if diversion && roll < guards+rescuers*DecoyShare {
			return Leurre
		}
		return Liberateur
	}
	return Chasseur
}

// roleRoll : tirage fixe de l'agent i, dérivé de la graine (splitmix64). Les
// rôles se recalculent sans consommer le générateur de la partie et un agent
// garde son rôle tant que les parts de son espèce ne changent pas.
func roleRoll(seed int64, i int) float64 {
	z := uint64(seed)*0x9E3779B97F4A7C15 + uint64(i+1)*0xD1B54A32D192ED03
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z ^= z >> 31
	return float64(z>>11) / (1 << 53)
}

func (w *World) assignRoles() {
	var g, r [SpeciesCount]float64
	for sp := range g {
		g[sp], r[sp] = w.roleBands(Species(sp))
	}
	for i := range w.Agents {
		a := &w.Agents[i]
		a.Role = pickRole(roleRoll(w.Seed, i), g[a.Species], r[a.Species], w.Rules.Diversion)
	}
}

// ---------------------------------------------------------------------------
// Évolution entre deux manches
// ---------------------------------------------------------------------------

// Evolve prépare les stratégies de la manche suivante : les perdants se
// rapprochent du vainqueur, tout le monde mute un peu. Déterministe par graine.
func Evolve(g [SpeciesCount]Genome, winner int, seed int64) [SpeciesCount]Genome {
	rng := rand.New(rand.NewPCG(uint64(seed), 0xe4011))
	out := g
	for s := range out {
		sigma := 0.5
		if winner >= 0 && s != winner {
			const k = 0.35
			o, v := &out[s], &g[winner]
			o.Guards += k * (v.Guards - o.Guards)
			o.Rescuers += k * (v.Rescuers - o.Rescuers)
			o.Caution += k * (v.Caution - o.Caution)
			o.Grit += k * (v.Grit - o.Grit)
			sigma = 1
		}
		out[s].Guards += rng.NormFloat64() * 0.02 * sigma
		out[s].Rescuers += rng.NormFloat64() * 0.03 * sigma
		out[s].Caution += rng.NormFloat64() * 0.08 * sigma
		out[s].Grit += rng.NormFloat64() * 0.08 * sigma
		out[s] = out[s].Clamp()
	}
	return out
}

// ---------------------------------------------------------------------------
// Événements et carte de chaleur
// ---------------------------------------------------------------------------

type EventKind uint8

const (
	EvNightFall     EventKind = iota + 1
	EvDawn                    //
	EvReversalSoon            //
	EvReversal                //
	EvUnguarded               // A : camp, B : gardiens présents, N : captifs
	EvEgg                     // A : espèce, N : captifs libérés
	EvPlume                   // A : espèce
	EvConvert                 // A : espèce d'origine, B : nouvelle espèce, N : nombre
	EvJailFull                // A : camp, N : palier franchi
	EvJailEmpty               // A : camp, N : pic de captifs
	EvEliminated              // A : espèce
	EvLeader                  // A : espèce
	EvPlayerCaught            // A : espèce du capteur
	EvPlayerCapture           // A : espèce de la proie
	EvPlayerFreed             //
	EvPlayerRescue            //
	EvEnd                     // A : vainqueur (-1 : égalité)
)

type Event struct {
	Tick int32     `json:"t"`
	Kind EventKind `json:"k"`
	A    int8      `json:"a"`
	B    int16     `json:"b"`
	N    int32     `json:"n"`
}

// emit écrit dans un tampon circulaire : aucune allocation pendant le tick.
func (w *World) emit(k EventKind, a, b, n int) {
	w.events[w.EventSeq%EventRing] = Event{Tick: int32(w.Tick), Kind: k, A: int8(a), B: int16(b), N: int32(n)}
	w.EventSeq++
}

// EventsSince ajoute à dst les événements émis depuis seq et renvoie le
// nouveau curseur. Un lecteur trop lent perd les plus anciens.
func (w *World) EventsSince(seq uint64, dst []Event) ([]Event, uint64) {
	if w.EventSeq-seq > EventRing {
		seq = w.EventSeq - EventRing
	}
	for ; seq < w.EventSeq; seq++ {
		dst = append(dst, w.events[seq%EventRing])
	}
	return dst, seq
}

var jailMilestones = [...]int{250, 500, 1000, 2000}

func (w *World) jailEvents() {
	for c := range w.held {
		n := w.held[c][0] + w.held[c][1] + w.held[c][2]
		w.heldPeak[c] = max(w.heldPeak[c], n)
		for _, m := range jailMilestones {
			if n >= m && w.jailMark[c] < m {
				w.jailMark[c] = m
				w.emit(EvJailFull, c, -1, m)
			}
		}
		if n == 0 && w.heldPeak[c] >= 30 {
			w.emit(EvJailEmpty, c, -1, w.heldPeak[c])
			w.heldPeak[c], w.jailMark[c] = 0, 0
		}
	}
}

func (w *World) addHeat(kind int, x, y float64) {
	cx := min(max(int(x/HeatCell), 0), HeatCols-1)
	cy := min(max(int(y/HeatCell), 0), HeatCols-1)
	w.Heat[kind][cy*HeatCols+cx]++
}

// ---------------------------------------------------------------------------
// Entrées : pilotage, réglages en direct, haies
// ---------------------------------------------------------------------------

type PlayerInput struct {
	DX, DY int8
	Sprint bool
}

// Command : message du navigateur. Les commandes de simulation (control, key,
// set, obstacle, unobstacle) sont enregistrées avec leur tick pour le replay.
type Command struct {
	Cmd     string  `json:"cmd"`
	I       int     `json:"i"`
	DX      int     `json:"dx"`
	DY      int     `json:"dy"`
	Sprint  bool    `json:"sprint"`
	Key     string  `json:"key"`
	Species int     `json:"species"`
	Value   float64 `json:"value"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	W       float64 `json:"w"`
	H       float64 `json:"h"`
	Tick    int     `json:"tick"`
	On      bool    `json:"on"`
	N       int     `json:"n"`
}

func sign(v int) int8 {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	}
	return 0
}

// Apply exécute une commande de simulation. Renvoie false si la commande n'en
// est pas une ou si elle est invalide (rien n'est alors modifié).
func (w *World) Apply(c Command) bool {
	switch c.Cmd {
	case "control":
		if c.I < -1 || c.I >= len(w.Agents) {
			return false
		}
		w.Player, w.Input = c.I, PlayerInput{}
		if c.I >= 0 {
			w.Agents[c.I].Resting = false
		}
	case "key":
		w.Input = PlayerInput{DX: sign(c.DX), DY: sign(c.DY), Sprint: c.Sprint}
	case "set":
		return w.set(c.Key, c.Species, c.Value)
	case "obstacle":
		return w.AddObstacle(c.X, c.Y, c.W, c.H)
	case "unobstacle":
		return w.RemoveObstacleAt(c.X, c.Y)
	default:
		return false
	}
	return true
}

func (w *World) set(key string, sp int, v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	switch key {
	case "vision":
		w.Tuning.Vision = v
		w.Tuning = w.Tuning.Clamp()
		return true
	case "speed":
		w.Tuning.Speed = v
		w.Tuning = w.Tuning.Clamp()
		return true
	}
	if sp < 0 || sp >= SpeciesCount {
		return false
	}
	g := &w.Genomes[sp]
	switch key {
	case "guards":
		g.Guards = v
	case "rescuers":
		g.Rescuers = v
	case "caution":
		g.Caution = v
	case "grit":
		g.Grit = v
	default:
		return false
	}
	*g = g.Clamp()
	w.assignRoles()
	return true
}

// AddObstacle pose une haie (hors des camps et des ponts). Les agents pris
// dedans en sortent d'eux-mêmes ; les bonus recouverts disparaissent.
func (w *World) AddObstacle(x, y, ow, oh float64) bool {
	if len(w.Obstacles) >= MaxObstacles || ow < 30 || oh < 30 || ow > 900 || oh > 900 ||
		x < WallMargin || y < WallMargin || x+ow > WorldSize-WallMargin || y+oh > WorldSize-WallMargin ||
		w.rectNearCamp(x, y, ow, oh, 40) || w.rectTouchesWater(x, y, ow, oh) {
		return false
	}
	w.Obstacles = append(w.Obstacles, Obstacle{X: x, Y: y, W: ow, H: oh})
	for k := range w.Items {
		if it := &w.Items[k]; it.Active && it.X >= x && it.X <= x+ow && it.Y >= y && it.Y <= y+oh {
			it.Active = false
		}
	}
	w.obstaclesChanged()
	return true
}

// RemoveObstacleAt retire la dernière haie posée sous le point (x, y).
func (w *World) RemoveObstacleAt(x, y float64) bool {
	for k := len(w.Obstacles) - 1; k >= 0; k-- {
		o := &w.Obstacles[k]
		if x >= o.X && x <= o.X+o.W && y >= o.Y && y <= o.Y+o.H {
			w.Obstacles = append(w.Obstacles[:k], w.Obstacles[k+1:]...)
			w.obstaclesChanged()
			return true
		}
	}
	return false
}

func (w *World) obstaclesChanged() {
	w.indexObstacles()
	w.computeFreeCells()
	w.buildCampFlows()
	w.buildPreyFlows()
	w.obsDirty = true
}

// TakeObstaclesChanged indique (une seule fois) que les haies ont changé.
func (w *World) TakeObstaclesChanged() bool {
	d := w.obsDirty
	w.obsDirty = false
	return d
}

// ---------------------------------------------------------------------------
// Instantanés (replay)
// ---------------------------------------------------------------------------

// Snapshot fige l'état d'une partie en fin de tick. Les caches (grilles,
// chemins, index des haies) ne sont pas copiés : Restore les reconstruit.
type Snapshot struct {
	Tick      int
	st        worldState
	agents    []Agent
	prevV     []float32
	obstacles []Obstacle
}

func (w *World) Snapshot() *Snapshot {
	return &Snapshot{
		Tick:      w.Tick,
		st:        w.worldState,
		agents:    append([]Agent(nil), w.Agents...),
		prevV:     append([]float32(nil), w.prevV...),
		obstacles: append([]Obstacle(nil), w.Obstacles...),
	}
}

func (w *World) Restore(s *Snapshot) {
	w.worldState = s.st
	copy(w.Agents, s.agents)
	copy(w.prevV, s.prevV)
	same := len(w.Obstacles) == len(s.obstacles)
	for k := 0; same && k < len(s.obstacles); k++ {
		same = w.Obstacles[k] == s.obstacles[k]
	}
	if !same {
		w.Obstacles = append(w.Obstacles[:0], s.obstacles...)
		w.indexObstacles()
		w.computeFreeCells()
		w.buildCampFlows()
		w.obsDirty = true
	}
	w.refreshStats()
	for sp := Species(0); sp < SpeciesCount; sp++ {
		w.jail[sp].Build(w.Agents, sp, true)
	}
	w.buildPreyFlows()
}
