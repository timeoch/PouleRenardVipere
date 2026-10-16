//go:build !naive_grid

package main

import "math"

const naiveGrid = false

// SpatialGrid range les indices d'un sous-ensemble d'agents par cellule de
// 50×50 dans une seule tranche contiguë (tri par comptage, O(N), 0 alloc en
// régime permanent). colPrefix permet de savoir en O(1) si un segment vertical
// de cellules est vide ; Cell.Start joue ce rôle pour les segments horizontaux.
// colPrefix[cy*GridCols+cx] compte les agents des cellules (cx, 0…cy-1).
type SpatialGrid struct {
	Cells     [GridCells]Cell
	Items     []int32
	cellOf    []int32
	colPrefix [(GridRows + 1) * GridCols]int32 // rangée par rangée : colPrefix[cy*GridCols+cx]
}

// Build indexe les agents de l'espèce sp, libres (prisoners=false) ou
// prisonniers (prisoners=true). Les indices d'une cellule sont croissants.
func (g *SpatialGrid) Build(agents []Agent, sp Species, prisoners bool) {
	n := len(agents)
	if naiveAlloc || cap(g.cellOf) < n {
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
	g.buildColPrefix()
}

// buildColPrefix parcourt les cellules dans l'ordre de la mémoire (rangée par
// rangée) : lectures et écritures séquentielles, une ligne de cache sert 8
// cellules au lieu d'une seule avec un parcours colonne par colonne.
func (g *SpatialGrid) buildColPrefix() {
	p := &g.colPrefix
	for cx := 0; cx < GridCols; cx++ {
		p[cx] = 0
	}
	for cy := 0; cy < GridRows; cy++ {
		row, prev, next := cy*GridCols, cy*GridCols, (cy+1)*GridCols
		for cx := 0; cx < GridCols; cx++ {
			p[next+cx] = p[prev+cx] + g.Cells[row+cx].Count
		}
	}
}

// BuildAll indexe les trois espèces en un seul passage sur les agents au lieu
// d'un passage par espèce : le tableau des agents (576 Ko) n'est lu qu'une
// fois. La seconde passe ne relit pas les agents mais tag, qui garde l'espèce
// et la cellule de chacun (36 Ko).
func BuildAll(gs *[SpeciesCount]SpatialGrid, agents []Agent, prisoners bool) {
	n := len(agents)
	for sp := range gs {
		g := &gs[sp]
		if naiveAlloc || cap(g.cellOf) < n {
			g.cellOf = make([]int32, n)
			g.Items = make([]int32, n)
		}
		g.Items = g.Items[:n]
		clear(g.Cells[:])
	}
	tag := gs[0].cellOf[:n] // espèce<<16 | cellule, -1 si l'agent n'est pas indexé
	var active [SpeciesCount]int
	for i := range agents {
		a := &agents[i]
		if a.Prisoner != prisoners {
			tag[i] = -1
			continue
		}
		c := int32(CellIndex(a.X, a.Y))
		tag[i] = int32(a.Species)<<16 | c
		gs[a.Species].Cells[c].Count++
		active[a.Species]++
	}
	for sp := range gs {
		cells := &gs[sp].Cells
		var start int32
		for i := range cells {
			cells[i].Start = start
			start += cells[i].Count
			cells[i].Count = 0
		}
	}
	for i, t := range tag {
		if t < 0 {
			continue
		}
		g := &gs[t>>16]
		cell := &g.Cells[t&0xFFFF]
		g.Items[cell.Start+cell.Count] = int32(i)
		cell.Count++
	}
	for sp := range gs {
		gs[sp].Items = gs[sp].Items[:active[sp]]
		gs[sp].buildColPrefix()
	}
}

// RowItems : agents des cellules cx0…cx1 de la rangée cy. Les cellules d'une
// rangée se suivent dans Items, d'où une seule tranche contiguë.
func (g *SpatialGrid) RowItems(cy, cx0, cx1 int) []int32 {
	a := &g.Cells[cy*GridCols+cx0]
	b := &g.Cells[cy*GridCols+cx1]
	if naiveAlloc {
		return append([]int32(nil), g.Items[a.Start:b.Start+b.Count]...)
	}
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
	return g.colPrefix[(yd+1)*GridCols+cx] - g.colPrefix[yt*GridCols+cx]
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
