//go:build !naive_layout

package main

const naiveLayout = false

// Agent tient dans une ligne de cache de 64 octets (vérifié par les tests) :
// champs triés par taille décroissante, aucun octet de remplissage entre eux.
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
