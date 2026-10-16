//go:build naive_grid

package main

import "math"

const naiveGrid = true

// SpatialGrid de la première version : une simple liste des agents de
// l'espèce. Chaque requête parcourt toute la liste, soit O(N) par agent et
// O(N²) par tick. Même interface que la grille de grid.go pour comparer les
// deux à code appelant identique.
//
//	go build -tags naive_grid .
type SpatialGrid struct {
	Items []int32
}

func (g *SpatialGrid) Build(agents []Agent, sp Species, prisoners bool) {
	if naiveAlloc {
		g.Items = nil
	}
	g.Items = g.Items[:0]
	for i := range agents {
		a := &agents[i]
		if a.Species == sp && a.Prisoner == prisoners {
			g.Items = append(g.Items, int32(i))
		}
	}
}

func BuildAll(gs *[SpeciesCount]SpatialGrid, agents []Agent, prisoners bool) {
	for sp := range gs {
		gs[sp].Build(agents, Species(sp), prisoners)
	}
}

// CellRange et RowItems : une seule « rangée » qui contient tout le monde.
func (g *SpatialGrid) CellRange(x, y, r float64) (cx0, cy0, cx1, cy1 int) {
	return 0, 0, 0, 0
}

func (g *SpatialGrid) RowItems(cy, cx0, cx1 int) []int32 {
	if naiveAlloc {
		return append([]int32(nil), g.Items...)
	}
	return g.Items
}

// Nearest : scan linéaire, le plus petit indice gagne à distance égale.
func (g *SpatialGrid) Nearest(agents []Agent, x, y, maxDist float64, accept func(*Agent) bool) (int, float64) {
	bestIdx := -1
	best2 := maxDist * maxDist
	for _, j := range g.Items {
		b := &agents[j]
		dx, dy := b.X-x, b.Y-y
		d2 := dx*dx + dy*dy
		if d2 > best2 || (d2 == best2 && bestIdx >= 0) || !accept(b) {
			continue
		}
		best2, bestIdx = d2, int(j)
	}
	if bestIdx < 0 {
		return -1, 0
	}
	return bestIdx, math.Sqrt(best2)
}
