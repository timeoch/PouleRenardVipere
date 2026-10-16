# Poule · Renard · Vipère • Arène de poursuite 2D

Moteur de simulation spatiale en temps réel du jeu de cour « Poule, Renard, Vipère » : trois équipes d'agents autonomes qui se chassent en cycle, font des prisonniers et tentent de les délivrer. Même architecture que ZombieHorde : un serveur de calcul en Go et une interface de télémétrie web servie via WebSocket, avec une graine déterministe.

Douze règles optionnelles pimentent le jeu de base (nuit, évasions, rivière, bonus, syndrome de Stockholm…). L'interface permet de suivre ou de piloter un agent, de poser des haies, de revenir en arrière, de régler les stratégies en direct, de faire évoluer les espèces de manche en manche et de mesurer l'équilibre du jeu par tournoi.

---

## 1. Spécifications & règles métier

### Jeu de base

- **Carte :**
  - Arène bornée de 5 000 × 5 000 unités entourée de murs infranchissables.
  - 40 haies et rochers rectangulaires infranchissables, générés par la graine.
  - 3 camps (rayon 230) aux sommets d'un triangle équilatéral : **Poulailler** (poules), **Terrier** (renards), **Nid** (vipères). Chaque espèce est ainsi à égale distance de sa proie et de son prédateur.

- **Chaîne alimentaire (cycle) :**
  - La **poule** attrape la **vipère**.
  - Le **renard** attrape la **poule**.
  - La **vipère** attrape le **renard**.

- **Population :** 3 × 3 000 agents par défaut, réglable de 100 à 10 000 individus par espèce. Les agents sont dispersés autour de leur camp au départ.
  - **Chasseurs (77 %)** : poursuivent la proie vulnérable la plus proche qu'ils voient (portée 420, ligne de vue non masquée par une haie ou l'eau). Sans cible, ils dérivent vers le centre de gravité de leurs proies.
  - **Libérateurs (15 %)** : tant que des coéquipiers sont prisonniers, ils marchent vers le camp adverse le plus proche qui en détient, puis sprintent vers le prisonnier le plus proche.
  - **Gardiens (8 %)** : font des rondes autour de leur camp et chassent les proies qui s'en approchent à moins de 700 unités, en particulier les libérateurs adverses.
  - Les parts de gardiens et de libérateurs sont réglables par espèce (voir § 5). Le rôle de chaque agent découle d'un tirage fixe dérivé de la graine : il ne change que si les parts de son espèce changent.

- **Déplacements :** pour rejoindre un camp ou un troupeau, les agents suivent un champ de distance calculé sur la grille. Ils contournent les haies et franchissent la rivière par les ponts au lieu de foncer en ligne droite.

- **Fuite :** un agent qui voit un prédateur à moins de 220 unités fuit. Près de son camp ou à court de souffle, il fuit en direction de son camp. Un prédateur épuisé n'effraie personne.

- **Refuge :** un agent est intouchable dans son propre camp et y récupère trois fois plus vite.

- **Capture & prison :** un prédateur au contact (14 unités) d'une proie vulnérable l'envoie dans la prison de **son** camp. Les prisonniers sont rangés en spirale de tournesol depuis le centre du camp. Les poules prisonnières sont donc au Terrier, gardées par les renards.

- **Libération :** un agent libre qui touche (18 unités) un coéquipier prisonnier en délivre **un seul**. Les deux repartent alors en **retour escorté** vers leur camp : 4 s d'immunité (anneau blanc), pendant lesquelles ils ne peuvent ni capturer ni libérer. Il n'y a pas de libération en chaîne.

- **Endurance & fatigue :** sprinter (chasse, fuite, sauvetage) coûte 1 point d'endurance par tick. Sous un seuil (25 points par défaut, réglable par la « ténacité »), l'agent cesse de chasser et récupère jusqu'à 80. À 0, il s'effondre, immobile et vulnérable, jusqu'à retrouver 50 points (~1,7 s).

- **Fin de manche :**
  - **Par élimination** : la partie s'arrête quand une seule espèce a encore des membres libres.
  - **Au coup de sifflet** (5 minutes simulées par défaut, soit 9 000 ticks ; réglable de 30 s à 30 minutes) : victoire de l'espèce qui compte le plus d'agents libres.

- **Déterminisme :** graine pseudo-aléatoire fixe (`MasterSeed = 42`), modifiable depuis l'interface. Une même graine avec les mêmes règles et les mêmes actions du joueur rejoue exactement la même partie.

### Règles optionnelles

Toutes sont actives par défaut. On les coche ou décoche dans le panneau « Règles de la manche », et elles s'appliquent au lancement suivant. Sans aucune règle, on retrouve le jeu de base.

| Règle | Clé | Effet |
|---|---|---|
| Cycle jour/nuit | `night` | 12 s de nuit par minute : vision réduite à 45 %, rayon de fuite à 65 %. |
| Évasions | `escape` | Une prison doit avoir un gardien éveillé pour 3 captifs à moins de 450 unités de son centre. Sinon, des captifs s'évadent (jusqu'à ~2,4 par seconde sans aucun gardien) et rentrent chez eux en retour escorté. |
| Force du nombre | `strength` | Une proie entourée d'au moins 2 alliés (45 unités) ne peut pas être capturée : le prédateur perd 15 points d'endurance et doit souffler. En fuite, les agents se rapprochent de leurs alliés. |
| Frénésie | `frenzy` | Après une capture, 2 s de sprint gratuit et 15 % plus rapide. |
| Syndrome de Stockholm | `stockholm` | Après 60 s de prison, un captif rejoint l'espèce de ses geôliers. Les effectifs des espèces changent alors en cours de manche. |
| Renversement | `reversal` | À mi-manche (2:30 pour une manche de 5 minutes), la chaîne alimentaire s'inverse (annonce 10 s avant). Les captifs restent dans la prison où ils sont. |
| Terrain | `terrain` | Rivière en Y partant d'un lac central, chaque bras séparant deux camps et franchissable sur deux ponts. Boue (vitesse × 0,55). Hautes herbes (vitesse × 0,85) où un agent n'est repéré qu'à moins de 70 unités, ce qui permet les embuscades. Le terrain a son propre générateur dérivé de la graine. |
| Bonus | `bonuses` | Un bonus apparaît toutes les 5 s (6 au plus). La **plume** donne 5 s de sprint gratuit. L'**œuf d'or** (6 % des bonus) libère d'un coup tous les captifs de l'espèce qui le ramasse. Deux paires de **terriers secrets** téléportent d'une entrée à l'autre, et les fuyards proches y plongent. |
| Chasse en meute | `pack` | Quand un congénère chasse à moins de 100 unités, deux chasseurs sur trois visent le flanc gauche ou droit de la proie pour la prendre en tenaille. |
| Anticipation | `lead` | Les chasseurs visent la position future de la proie (vitesse du tick précédent × temps de rattrapage, 25 ticks au plus). |
| Diversion | `diversion` | 30 % des libérateurs deviennent des **leurres** : près d'une prison, ils vont provoquer le gardien le plus proche sans entrer dans sa zone de capture, puis l'entraînent au loin en fuyant. |
| Rôles dynamiques | `dynamic` | Les rôles sont recalculés chaque seconde. La part de libérateurs augmente avec la proportion de membres captifs (jusqu'à +50 points), celle des gardiens avec le remplissage de la prison du camp (jusqu'à +30 points). |

## 2. Structure du projet

```text
.
├── index.html          # Interface web (Canvas 2D, caméra, télémétrie, panneaux), embarquée dans le binaire
├── main.go             # Paramètres, monde, boucle de simulation, décisions des agents, lancement
├── agent.go            # Structure Agent (64 octets) ; agent_naive.go : version d'origine (88 octets)
├── grid.go             # Grille spatiale ; grid_naive.go : recherche linéaire O(N²) d'origine
├── alloc.go            # Interrupteur des allocations d'origine (alloc_naive.go)
├── trig.go             # sincos en un appel ; trig_naive.go : math.Sin et math.Cos séparés
├── workers.go          # Phase parallèle : pool de workers borné, lots, générateur par agent
├── rules.go            # Règles optionnelles, terrain, champs de distance, rôles, évolution, entrées, instantanés
├── session.go          # Session WebSocket : ligne de temps (replay), trames binaires, commandes
├── tournament.go       # Parties sans interface en parallèle (tournoi, mode -ticks pour hyperfine)
├── store.go            # Persistance PostgreSQL des tournois, cache LRU, API /api/courbe
├── *_test.go           # Tests et benchmarks (simulation, trames, tournoi, base de données)
├── tools/              # wsload (charge WebSocket), hfstats (statistiques hyperfine), flamegraph (SVG annotés)
├── run_benchmarks.sh   # Toutes les mesures du rapport en une commande (Makefile : make bench)
├── docker-compose.yml  # PostgreSQL 17 pour les mesures de persistance
├── constitution.md     # Règles imposées aux assistants de code sur ce dépôt
├── RAPPORT_AUDIT.pdf   # Rapport d'audit de performance
├── go.mod
└── go.sum
```

Les fichiers `*_naive.go` ne servent qu'aux mesures : ils remettent les choix de la première version derrière des tags de build (`go build -tags naive_grid .`), pour chiffrer chaque optimisation séparément.

## 3. Prérequis

- Go 1.27.1 ou supérieur (version déclarée dans `go.mod`, comme pour ZombieHorde).
- Un navigateur moderne compatible WebSocket et Canvas HTML5.

## 4. Installation & lancement

```bash
go mod download
go run .
```

Puis ouvrir **http://localhost:8081** (le port 8081 permet de faire tourner ZombieHorde en parallèle sur 8080).

Chaque onglet ouvre sa propre partie. Les champs « Seed », « Individus par espèce » et « Durée (minutes) », puis le bouton « Relancer » (ou Entrée), démarrent une nouvelle partie. Les règles cochées et les réglages en cours sont conservés.

Le coût d'un tick croît plus vite que l'effectif, car les agents sont plus serrés. Au-delà de 5 000 individus par espèce, il dépasse le budget de 33 ms et la simulation ralentit (voir § 6).

Options de la ligne de commande :

```bash
go run . -seed 7                       # graine de départ des parties
go run . -individus 1000 -duree 2      # effectif par espèce et durée d'une manche (minutes) par défaut
go run . -regles night,terrain,escape  # règles par défaut des parties (toutes, aucune, ou une liste de clés)
go run . -port 8090                    # autre port que 8081
go run . -tournoi 100 -regles aucune   # 100 parties sans interface, en parallèle, puis le bilan
go run . -workers 4                    # workers de la phase parallèle d'une partie (défaut : la moitié des cœurs logiques)
go run . -ticks 300 -workers 1         # une partie sans interface pendant 300 ticks, chronométrée (hyperfine)
go run . -trames json                  # trames JSON d'origine au lieu du format binaire
go run . -pprof localhost:6060         # profileur net/http/pprof sur un port d'administration séparé
go run . -tournoi 40 -db postgres://prv:prv@localhost:5434/prv   # enregistre les parties et leur télémétrie
```

Avec `-db`, le serveur expose aussi `GET /api/courbe?run=ID&de=0&a=9000` (télémétrie d'une partie, servie par un cache LRU).

Au premier chargement, la page reprend la configuration du serveur ; ensuite, celle choisie dans l'interface.

## 5. Interface

### Carte

- **Molette** : zoom (jusqu'à × 16) autour du curseur. **Glisser** : déplacer la vue. Boutons −, + et « Vue entière » en haut de la carte.
- **Suivre un agent** : un clic sur un agent zoome dessus et la caméra le suit. La fiche « Agent suivi » affiche son espèce, son rôle, ce qu'il fait (avec une ligne vers sa cible), son endurance, son bonus et son temps de prison. Échap arrête le suivi.
- **Piloter un agent** : bouton « Le contrôler » de la fiche. ZQSD (ou WASD, ou les flèches) pour bouger, Maj pour sprinter, Échap pour rendre la main à l'IA. L'agent piloté capture et délivre au contact comme les autres.
- **Poser une haie** : outil « Poser une haie », puis glisser pour tracer un rectangle. Un clic droit sur une haie la retire. Les camps, la rivière et les ponts sont protégés. Les agents pris dans une nouvelle haie en sortent d'eux-mêmes, et les chemins sont recalculés.
- **Carte de chaleur** : lieux des captures ou des libérations depuis le début de la manche.
- Sur la carte, chaque espèce a sa couleur et sa forme : poule = disque bleu, renard = triangle orange, vipère = losange vert (les trois couleurs passent le contrôle daltonisme sur toutes les paires). Forme pâle = épuisé, anneau blanc = retour escorté, anneau doré = sprint gratuit, petits points dans un camp = prisonniers. Plume blanche et œuf doré = bonus. Cercles bruns reliés en pointillés = terriers secrets. La nuit, la carte s'assombrit.

### Ligne de temps (replay)

Sous la carte, le curseur ramène à n'importe quel instant de la manche, et « −10 s » recule de 10 secondes. La partie se met en pause. « Reprendre » rejoue la suite à l'identique, actions du joueur comprises, et « En direct » revient là où la partie en était. Une action faite dans le passé (pilotage, haie, réglage) ouvre une nouvelle branche : la suite enregistrée est abandonnée.

Vitesse de simulation : ¼×, ½×, 1×, 2× ou 4×. La barre d'espace met en pause (hors pilotage).

### Panneaux

| Panneau | Contenu |
|---|---|
| Seed déterministe | Graine, individus par espèce, durée de la manche, Relancer, Pause, vitesse. |
| TPS / FPS | Ticks de simulation par seconde côté serveur (cible : 30 × la vitesse) / images redessinées par seconde côté navigateur. |
| Manche | Temps simulé sur la durée de la manche, génération en mode évolution, état (nuit, chaîne inversée, retard du replay). |
| Coût du tick | Temps de calcul moyen d'un tick (moyenne glissante), comparé au budget de 33,3 ms. |
| Effectifs | Par espèce : agents libres, prisonniers, épuisés. Bilan : captures, libérations (œufs d'or compris), évasions, recrues (syndrome de Stockholm). |
| Agents libres dans le temps | Courbe des effectifs libres par espèce, avec infobulle au survol. |
| Journal | Événements marquants : nuit, renversement, prison mal gardée, œuf d'or, passages de captifs, paliers et vidange des prisons, espèce en tête, éliminations, actions de l'agent piloté. Les plus importants s'affichent aussi en bandeau sur la carte. |
| Pari | Pariez sur l'espèce gagnante avant 0:10. La cote vaut × 3, ou découle des résultats du dernier tournoi (au moins 6 parties). Le score est conservé dans le navigateur. |
| Règles de la manche | Les 12 règles optionnelles, appliquées au lancement suivant. |
| Réglages en direct | Vision et vitesse globales. Par espèce : part de gardiens et de libérateurs, prudence (rayon de fuite) et ténacité (seuil d'endurance sous lequel on arrête de chasser). Les réglages sont enregistrés dans la ligne de temps et rejoués par le replay. |
| Évolution | « Enchaîner les manches » : 5 s après chaque fin de manche, une nouvelle manche démarre sur la graine suivante. Les perdants copient 35 % de l'écart avec la stratégie du vainqueur, puis tout le monde mute un peu. Le tableau montre les stratégies en cours, avec l'historique des vainqueurs. |
| Tournoi | 11 à 99 parties sans affichage sur tous les cœurs sauf un, avec les règles, réglages et stratégies de la partie en cours. Affiche la part de victoires par espèce, les égalités, la durée moyenne et la part de victoires par élimination. |

## 6. Tests & mesures

```bash
go test -short .                     # déterminisme, parallèle = séquentiel, invariants, replay, chemins, rôles, évolution, grille, trames
go test -run TestFullGameTimeline -v .   # partie complète (toutes les règles), coût du tick par tranche de 20 s
./run_benchmarks.sh                  # toutes les mesures du rapport d'audit (voir RAPPORT_AUDIT.pdf), résultats dans bench/
RAPIDE=1 ./run_benchmarks.sh cpu     # version courte, pour vérifier que tout tourne
```

Les tests vérifient notamment :

- **Déterminisme**, avec et sans les règles optionnelles, et **parallélisme** : 1, 2, 3, 8 ou 12 workers jouent exactement la même partie.
- **Invariants**, sur le jeu classique et avec toutes les règles :
  - les prisonniers sont dans la bonne prison et aucun agent libre ne se trouve dans un mur ou dans l'eau ;
  - l'effectif total est conservé, ainsi que celui de chaque espèce sans le syndrome de Stockholm ;
  - les statistiques et les décomptes par camp sont cohérents.
- **Replay** : revenir en arrière puis rejouer redonne exactement la même partie, entrées du joueur comprises, et une nouvelle entrée ouvre bien une autre branche.
- **Chemins** : sur 20 graines, chaque camp reste accessible depuis les deux autres malgré la rivière, et suivre le champ mène bien au camp.
- **Grille** : 67 500 requêtes du plus proche voisin donnent exactement le résultat d'un parcours linéaire.
- **Trames binaires** : une trame relue redonne exactement l'en-tête et les agents d'origine.
- **Base de données** (avec `PRV_DB`) : une partie enregistrée puis relue donne les mêmes lignes.

Mesures de référence (Intel i5-12450H sur secteur, Go 1.27.1, seed 42, médianes de 10 passes ; détail et protocole dans `RAPPORT_AUDIT.pdf`) :

| Mesure | Valeur |
|---|---|
| `sizeof(Agent)` | 64 octets, soit une ligne de cache |
| Tick, début de partie (toutes les règles) | 1,92 ms sur 1 worker, 0,88 ms sur 4 workers, 0 alloc/op |
| Tick, mi-partie (t = 1 500, toutes les règles) | 1,79 ms sur 1 worker, 1,09 ms sur 4 workers, 0 alloc/op |
| Tick, mi-partie (t = 1 500, jeu classique) | 1,6 à 1,7 ms sur 1 worker, 0,90 ms sur 4 workers |
| Version naïve O(N²) (tags `naive_*`), début de partie | 135,8 ms, 106 Mo alloués et 47 cycles de GC par tick |
| Passage à l'échelle | ×2,0 à 4 workers, plus rien au-delà (4 cœurs performants, 18 % du tick reste séquentiel) |
| Trame envoyée au navigateur | binaire : 53,5 Kio en 23 µs ; JSON : 105 Kio en 398 µs |
| Tournoi (parties de 10 s) | ×5,1 sur 11 parties en parallèle ; 40 parties de 5 min en 204 s |
| Télémétrie en base | `COPY` : 209 000 lignes/s (281 en INSERT ligne à ligne) ; courbe d'une partie : 0,9 ms avec index, 28 ms sans |

Le coût du tick affiché par le serveur est plus élevé que dans le benchmark. Entre deux ticks, le processeur baisse sa fréquence ou bascule la tâche sur un cœur économe. Sur batterie, tout est 2 à 3 fois plus lent. Comparer des médianes de plusieurs passes, sur secteur, jamais une mesure isolée.

## 7. Choix de performance

- **Agents contigus** : `[]Agent` de 64 octets, espèces mélangées dans le tableau pour qu'aucune ne soit favorisée par l'ordre de traitement. Les nouveaux champs (temps de prison, camp de détention, sprint gratuit) logent dans le remplissage de la ligne de cache, à la place de l'ancien numéro de place en prison.
- **Grille spatiale 1D contiguë** (100 × 100 cellules de 50 unités) : une grille d'agents libres et une grille de prisonniers par espèce, reconstruites en O(N) par tri par comptage, sans allocation. La recherche du plus proche voisin explore des anneaux de cellules et saute en O(1) les segments vides grâce à des sommes préfixes. `TestGridNearestMatchesBruteForce` vérifie qu'elle renvoie exactement le même agent qu'un scan linéaire.
- **Balayage par rangées** : les cellules d'une rangée se suivent dans le tableau d'indices. Les recherches de voisinage (menaces, troupeau, alliés, gardiens) parcourent donc une seule tranche par rangée au lieu d'une par cellule.
- **Champs de distance précalculés** : un parcours en largeur sur la grille donne, pour chaque cellule, le voisin à rejoindre vers chaque camp, recalculé seulement quand les haies changent. Il donne aussi le chemin vers le troupeau de chaque espèce, recalculé deux fois par seconde. Suivre un chemin coûte une lecture de tableau par agent.
- **Obstacles et terrain indexés par cellule** : un test de collision ne regarde que le type de terrain et les obstacles qui recouvrent la cellule du point.
- **Décision puis déplacement, en parallèle** : chaque agent décide à partir de l'état de début de tick et n'écrit que dans sa propre entrée ; ce que lisent ses voisins (immunité, épuisement) est mis à jour hors de la phase parallèle. Décision, déplacement et recherche des captures tournent sur un pool de workers partagé par toutes les parties du processus (borné à GOMAXPROCS − 1), par lots de 128 agents distribués avec un compteur atomique. Chaque agent tire ses nombres aléatoires d'un splitmix64 dérivé de (graine, tick, agent) : la partie est identique quel que soit le nombre de workers (`TestParallelMatchesSequential`). Les captures sont appliquées ensuite dans l'ordre des agents, ce qui donne exactement le résultat de la boucle séquentielle.
- **Zéro allocation par tick** : les événements passent par un tampon circulaire et les rôles par un tirage dérivé de la graine (splitmix64), sans consommer le générateur de la partie.
- **Replay par instantanés** : toutes les 5 s simulées, la session copie l'état de valeur du monde, les agents et les haies (~0,7 Mo pour 9 000 agents). Pour les grandes et longues parties, les instantanés s'espacent afin de rester sous ~150 Mo : au pire un toutes les 30 s, pour 30 000 agents pendant 30 minutes. Le générateur PCG de `math/rand/v2` se copie par valeur. Revenir au tick t restaure le dernier instantané, puis rejoue les ticks manquants en réappliquant les entrées enregistrées. Les caches (grilles, chemins, index des haies) sont reconstruits à l'identique.
- **Envoi découplé** : la boucle de simulation publie la dernière trame et un envoyeur séparé la transmet. Un navigateur lent perd des trames mais ne ralentit jamais la simulation. Les messages de contrôle (début de manche, tournoi) passent en priorité et ne sont jamais perdus.
- **Trames binaires** : un en-tête JSON court (statistiques, événements, agent suivi), puis les agents en triplets int16 copiés tels quels depuis la mémoire. Le navigateur pose un `Int16Array` sur le tampon reçu, sans décodage. Les tampons sont recyclés par un `sync.Pool` entre la boucle et l'envoyeur. `-trames json` remet le format d'origine pour comparer.
- **Persistance des tournois** (`-db`) : télémétrie de chaque tick écrite en un seul `COPY` par partie, lue par une requête qui passe par l'index `(run_id, tick)`, avec un cache LRU borné devant la base.
- **Rendu par pixels sur deux calques** : le décor (terrain, haies, camps, chaleur) n'est redessiné que quand la vue change. Les agents sont tamponnés via un masque précalculé dans un tampon transparent, au lieu de tracer 9 000 formes vectorielles.

### Pistes d'exercices

- Réduire la partie séquentielle du tick (reconstruction des grilles, libérations, statistiques), qui limite le gain du parallélisme au-delà de 4 workers (loi d'Amdahl, voir le rapport).
- Lancer l'évolution dans le tournoi : plusieurs générations sans interface, pour voir vers quelles stratégies convergent les espèces.
