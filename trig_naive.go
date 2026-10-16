//go:build naive_trig

package main

import "math"

// sincos de la première version : deux appels séparés sur le même angle.
//
//	go build -tags naive_trig .
func sincos(a float64) (sin, cos float64) { return math.Sin(a), math.Cos(a) }
