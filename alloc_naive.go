//go:build naive_alloc

package main

// Allocations de la première version, réintroduites pour la mesure. Les
// grilles réallouent leurs tranches à chaque reconstruction, RowItems renvoie
// une copie défensive des indices au lieu d'une vue, la file du parcours en
// largeur repart de zéro et grandit par append, et les vitesses du tick
// précédent sont copiées dans une tranche neuve.
//
//	go build -tags naive_alloc .
const naiveAlloc = true
