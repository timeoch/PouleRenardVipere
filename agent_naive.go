//go:build naive_layout

package main

const naiveLayout = true

// Agent dans l'ordre « logique » de la première version : les champs sont
// groupés par sens et non par taille. Chaque petit champ placé devant un
// float64 coûte jusqu'à 7 octets de remplissage : 88 octets au lieu de 64, et
// la plupart des agents chevauchent deux lignes de cache.
//
//	go build -tags naive_layout .
type Agent struct {
	Species   Species
	X, Y      float64
	Prisoner  bool
	VX, VY    float64 // vitesse décidée pour le tick courant
	Exhausted bool
	Stamina   int16
	Speed     float64
	Resting   bool
	WanderAng float64
	Role      Role
	Immune    int16   // ticks d'immunité restants
	JailTicks uint16  // ticks passés en prison (syndrome de Stockholm)
	Jail      Species // camp où l'agent est détenu
	Boost     uint8   // ticks de sprint gratuit restants (frénésie, plume)
}
