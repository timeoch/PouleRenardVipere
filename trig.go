//go:build !naive_trig

package main

import "math"

// sincos : sinus et cosinus d'un même angle en un seul appel. math.Sincos ne
// fait qu'une fois la réduction de l'argument et partage les calculs des deux
// polynômes. Les agents en errance tirent leur direction de WanderAng à chaque
// tick. Mesuré : math.Sin + math.Cos = 5,8 % du tick (profil V3). En A/B
// croisé (benchstat, n = 20), Sincos gagne ~3 % en début de partie
// (p = 0,002) ; en milieu de partie, l'écart (−1 à −2 %) reste à la limite
// du bruit.
func sincos(a float64) (sin, cos float64) { return math.Sincos(a) }
