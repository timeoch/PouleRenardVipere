//go:build !naive_alloc

package main

// naiveAlloc remet les allocations de la première version (voir
// alloc_naive.go). Constante : le compilateur supprime les branches mortes,
// la version optimisée ne paie aucun test.
const naiveAlloc = false
