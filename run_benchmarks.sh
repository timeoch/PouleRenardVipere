#!/usr/bin/env bash
# Toutes les mesures du rapport d'audit en une commande.
#
#   ./run_benchmarks.sh            tout : cpu puis sql (~40 min)
#   ./run_benchmarks.sh cpu        simulation, réseau, profils, hyperfine
#   ./run_benchmarks.sh sql        PostgreSQL (Docker) : COPY, EXPLAIN ANALYZE, cache
#   ./run_benchmarks.sh profils    profils, flamegraphs et charge réseau seulement
#   ./run_benchmarks.sh flammes    flamegraphs seulement, à partir des profils existants
#   ./run_benchmarks.sh complements  A/B croisés et variantes instrumentées (inclus dans cpu)
#   RAPIDE=1 ./run_benchmarks.sh   passes réduites, pour vérifier que tout tourne
#
# Résultats dans bench/ : sorties brutes de go test, comparaisons benchstat,
# exports hyperfine, profils pprof et flamegraphs SVG annotés.
#
# Avant de lancer : secteur branché, mode d'alimentation « performances »,
# navigateur et applications lourdes fermés, Docker arrêté pour la partie cpu.
set -euo pipefail
cd "$(dirname "$0")"

OUT=bench
WORKERS=${WORKERS:-4} # cœurs performants de l'i5-12450H, optimum de BenchmarkWorkers
DB=${PRV_DB:-postgres://prv:prv@localhost:5434/prv}
if [ -n "${RAPIDE:-}" ]; then
	COUNT=3 RUNS=3 WARMUP=1 TICKS=150 PROF=2s
else
	COUNT=10 RUNS=10 WARMUP=3 TICKS=300 PROF=5s
fi
EXE=
case "$(uname -s)" in MINGW* | MSYS* | CYGWIN*) EXE=.exe ;; esac
export PATH="$PATH:$(go env GOPATH)/bin"

step() { printf '\n== %s\n' "$*"; }
need() { command -v "$1" >/dev/null || {
	echo "outil manquant : $1 ($2)"
	exit 1
}; }

# Versions du journal d'optimisation, dans l'ordre où le profil les a
# désignées. Chaque version retire un tag « naïf » à la précédente.
# V5 est le binaire de V4 lancé avec $WORKERS workers.
VERSIONS="v0 v1 v2 v3 v4"
tags_of() {
	case $1 in
	v0) echo naive_layout,naive_grid,naive_alloc,naive_trig ;; # version naïve complète
	v1) echo naive_layout,naive_alloc,naive_trig ;;            # + grille spatiale
	v2) echo naive_layout,naive_trig ;;                        # + zéro allocation
	v3) echo naive_trig ;;                                     # + structure de 64 octets
	v4) echo "" ;;                                             # + sincos
	esac
}

env_info() {
	{
		echo "date : $(date '+%Y-%m-%d %H:%M')"
		go version
		hyperfine --version
		case "$(uname -s)" in
		MINGW* | MSYS* | CYGWIN*)
			powershell.exe -NoProfile -Command '
				Get-CimInstance Win32_Processor | Select-Object Name,NumberOfCores,NumberOfLogicalProcessors,MaxClockSpeed,L2CacheSize,L3CacheSize | Format-List
				Get-CimInstance Win32_CacheMemory | Select-Object Purpose,InstalledSize | Format-Table
				Get-CimInstance Win32_PhysicalMemory | Select-Object Capacity,Speed,PartNumber | Format-Table
				$os = Get-CimInstance Win32_OperatingSystem; "$($os.Caption) $($os.Version)"
				Get-CimInstance Win32_Battery | Select-Object BatteryStatus,EstimatedChargeRemaining | Format-List
				powercfg /getactivescheme
				Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Control\Power\User\PowerSchemes" | Select-Object ActiveOverlayAcPowerScheme | Format-List'
			;;
		Linux) lscpu && free -h && uname -a ;;
		Darwin) sysctl -n machdep.cpu.brand_string && sysctl hw.l1dcachesize hw.l2cachesize hw.l3cachesize hw.memsize ;;
		esac
	} >"$OUT/env.txt" 2>&1
	cat "$OUT/env.txt"
}

# Profils : début de partie (BenchmarkUpdate/Debut), sans préchauffage, pour
# que le profil ne contienne que les ticks mesurés. FOCUS garde la boucle
# mesurée, les workers du pool et les goroutines de fond du ramasse-miettes.
FOCUS='benchTicks|poolJobs|gcBgMarkWorker|bgsweep|bgscavenge'

# flame NOM FICHIER_TRACES TITRE NOTES… : flamegraph SVG annoté.
flame() {
	local name=$1 traces=$2 title=$3
	shift 3
	local args=()
	for n in "$@"; do args+=(-note "$n"); done
	go run ./tools/flamegraph -in "$traces" -out "$OUT/prof/$name.svg" -focus "$FOCUS" \
		-title "$title" -subtitle "i5-12450H, go test -bench BenchmarkUpdate/Debut" "${args[@]}"
}

profiles() {
	for v in $VERSIONS v5; do
		local bin=$v w=1
		if [ $v = v5 ]; then bin=v4 w=$WORKERS; fi
		"$OUT/bin/$bin.test$EXE" -test.run '^$' -test.bench 'BenchmarkUpdate/Debut' -test.benchtime $PROF \
			-test.count 1 -test.benchmem -workers $w -test.cpuprofile "$OUT/prof/$v.cpu.pprof" \
			-test.memprofile "$OUT/prof/$v.mem.pprof" >"$OUT/prof/$v.bench.txt"
		local p="go tool pprof -focus $FOCUS"
		$p -top -cum -nodecount 40 "$OUT/bin/$bin.test$EXE" "$OUT/prof/$v.cpu.pprof" >"$OUT/prof/$v.cpu.top.txt" 2>/dev/null
		$p -top -nodecount 25 "$OUT/bin/$bin.test$EXE" "$OUT/prof/$v.cpu.pprof" >"$OUT/prof/$v.cpu.flat.txt" 2>/dev/null
		$p -sample_index=alloc_space -top -nodecount 15 "$OUT/bin/$bin.test$EXE" "$OUT/prof/$v.mem.pprof" \
			>"$OUT/prof/$v.mem.top.txt" 2>/dev/null
		go tool pprof -traces "$OUT/bin/$bin.test$EXE" "$OUT/prof/$v.cpu.pprof" >"$OUT/prof/$v.cpu.traces" 2>/dev/null
		go tool pprof -sample_index=alloc_space -traces "$OUT/bin/$bin.test$EXE" "$OUT/prof/$v.mem.pprof" \
			>"$OUT/prof/$v.mem.traces" 2>/dev/null
	done
	flammes
}

# flammes : flamegraphs SVG annotés, à partir des profils déjà capturés.
flammes() {
	local gc='^(runtime.mallocgc|runtime.memmove|runtime.gcBgMarkWorker|runtime.memclrNoHeapPointers|runtime.bgsweep)$=Allocateur, copies et ramasse-miettes'
	local serial='^(BuildAll|World..prepare|World..rescues|World..refreshStats|World..buildPreyFlows|World..prisonTime|World..escapes|World..assignRoles|World..pickups|World..checkEnd)$'
	flame v0.cpu "$OUT/prof/v0.cpu.traces" "V0 naïve : profil CPU (9 000 agents, 1 worker)" \
		'SpatialGrid..Nearest$=Plus proche voisin : parcours de toute l’espèce à chaque requête (O(N²))' \
		'(World..threat|World..herd|World..alliesAround|World..guardsNear)$=Voisins dans un rayon : même parcours complet' \
		"$gc"
	flame v0.mem "$OUT/prof/v0.mem.traces" "V0 naïve : octets alloués" \
		'SpatialGrid..RowItems=Copie défensive des voisins (RowItems)' \
		'(SpatialGrid..Build|BuildAll)$=Grilles réallouées à chaque reconstruction'
	flame v1.cpu "$OUT/prof/v1.cpu.traces" "V1 (grille spatiale) : profil CPU, 1 worker" \
		"$gc" \
		'SpatialGrid..Nearest$=Plus proche voisin (grille)'
	flame v3.cpu "$OUT/prof/v3.cpu.traces" "V3 (zéro allocation, 64 octets) : profil CPU, 1 worker" \
		'^(math.Cos|math.Sin)$=math.Cos et math.Sin appelés séparément sur le même angle' \
		'SpatialGrid..Nearest$=Plus proche voisin (grille)'
	flame v4.cpu "$OUT/prof/v4.cpu.traces" "V4 (sincos) : profil CPU, 1 worker" \
		'(World..parallel)$=Phases parallélisables (décision, déplacement, recherche des captures)' \
		"$serial=Principales étapes séquentielles : grilles, préparation, libérations, statistiques"
	flame v5.cpu "$OUT/prof/v5.cpu.traces" "V5 (pool de $WORKERS workers) : profil CPU" \
		'(batch..work)$=Phases parallèles, réparties sur les workers' \
		"$serial=Étapes séquentielles : pendant ce temps, les autres workers attendent"
}

# load FORMAT : serveur réel avec 8 clients WebSocket pendant 15 s, profil CPU
# du serveur capturé pendant la charge sur le port d'administration.
load() {
	local fmt=$1 port=8095 admin=localhost:6065
	"$OUT/bin/v4$EXE" -port $port -trames "$fmt" -workers $WORKERS -pprof $admin >"$OUT/server_$fmt.log" 2>&1 &
	local pid=$!
	sleep 2
	curl -s -o "$OUT/prof/server_$fmt.cpu.pprof" "http://$admin/debug/pprof/profile?seconds=15" &
	local cpid=$!
	"$OUT/bin/wsload$EXE" -url ws://localhost:$port/ws -clients 8 -duree 15s -chauffe 1s | tee "$OUT/load_$fmt.txt"
	wait $cpid || true
	kill $pid 2>/dev/null || taskkill //F //PID $pid >/dev/null 2>&1 || true
	wait $pid 2>/dev/null || true
	go tool pprof -top -cum -nodecount 60 "$OUT/bin/v4$EXE" "$OUT/prof/server_$fmt.cpu.pprof" >"$OUT/prof/server_$fmt.top.txt" 2>/dev/null
}

cpu() {
	need hyperfine "https://github.com/sharkdp/hyperfine"
	need benchstat "go install golang.org/x/perf/cmd/benchstat@latest"
	need fieldalignment "go install golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment@latest"
	mkdir -p "$OUT/bin" "$OUT/prof"

	step "Environnement"
	env_info

	step "Tests : déterminisme, parallèle = séquentiel, invariants, trames"
	go test -short . | tee "$OUT/tests.txt"

	step "Compilation des versions"
	for v in $VERSIONS; do
		go build -tags "$(tags_of $v)" -o "$OUT/bin/$v$EXE" .
		go test -c -tags "$(tags_of $v)" -o "$OUT/bin/$v.test$EXE" .
		"$OUT/bin/$v.test$EXE" -test.run TestAgentFitsCacheLine -test.v | grep sizeof | sed "s/^/$v : /"
	done | tee "$OUT/sizeof.txt"
	go build -o "$OUT/bin/wsload$EXE" ./tools/wsload

	step "Analyse statique : alignement des champs et échappement"
	{
		echo "# fieldalignment, version naïve (tag naive_layout)"
		GOFLAGS=-tags=naive_layout fieldalignment . 2>&1 | grep -i 'agent_naive' || true
		echo "# fieldalignment, version optimisée"
		fieldalignment . 2>&1 | grep -i 'agent.go' || echo "agent.go : aucun octet perdu"
	} | tee "$OUT/fieldalignment.txt"
	go build -gcflags=-m -o "$OUT/bin/escape$EXE" . 2>&1 | grep -E '(workers|grid|main)\.go' >"$OUT/escape.txt" || true

	step "BenchmarkUpdate par version ($COUNT passes de 30 ticks, 1 worker ; V5 = V4 avec $WORKERS workers)"
	for v in $VERSIONS; do
		"$OUT/bin/$v.test$EXE" -test.run '^$' -test.bench BenchmarkUpdate -test.benchtime 30x \
			-test.count $COUNT -test.benchmem -workers 1 | tee "$OUT/$v.txt"
	done
	"$OUT/bin/v4.test$EXE" -test.run '^$' -test.bench BenchmarkUpdate -test.benchtime 30x \
		-test.count $COUNT -test.benchmem -workers $WORKERS | tee "$OUT/v5.txt"
	(cd "$OUT" && benchstat v0.txt v1.txt v2.txt v3.txt v4.txt v5.txt) | tee "$OUT/benchstat_versions.txt"
	(cd "$OUT" && benchstat v0.txt v5.txt) >"$OUT/benchstat_v0_v5.txt"
	for k in 1 2 3 4; do
		(cd "$OUT" && benchstat v$k.txt v$((k + 1)).txt) >"$OUT/benchstat_v${k}_v$((k + 1)).txt"
	done
	(cd "$OUT" && benchstat v0.txt v1.txt) >"$OUT/benchstat_v0_v1.txt"

	step "Passage à l'échelle, taille des lots, goroutine par agent"
	"$OUT/bin/v4.test$EXE" -test.run '^$' -test.bench BenchmarkWorkers -test.benchtime 100x -test.count $COUNT | tee "$OUT/workers.txt"
	benchstat "$OUT/workers.txt" >"$OUT/benchstat_workers.txt"
	"$OUT/bin/v4.test$EXE" -test.run '^$' -test.bench BenchmarkChunk -test.benchtime 100x -test.count 5 | tee "$OUT/chunk.txt"
	benchstat "$OUT/chunk.txt" >"$OUT/benchstat_chunk.txt"
	"$OUT/bin/v4.test$EXE" -test.run '^$' -test.bench BenchmarkSpawnPerAgent -test.benchtime 30x \
		-test.count $COUNT -test.benchmem | tee "$OUT/spawn.txt"

	step "Alignement selon l'effectif : 88 octets (v2) contre 64 octets (v3)"
	"$OUT/bin/v2.test$EXE" -test.run '^$' -test.bench BenchmarkPopulation -test.benchtime 30x -test.count $COUNT -workers 1 >"$OUT/pop_88.txt"
	"$OUT/bin/v3.test$EXE" -test.run '^$' -test.bench BenchmarkPopulation -test.benchtime 30x -test.count $COUNT -workers 1 >"$OUT/pop_64.txt"
	(cd "$OUT" && benchstat pop_88.txt pop_64.txt) | tee "$OUT/benchstat_population.txt"

	step "Trames réseau : JSON contre binaire"
	"$OUT/bin/v4.test$EXE" -test.run TestBinaryFrameRoundTrip -test.v | tee "$OUT/frames_roundtrip.txt"
	"$OUT/bin/v4.test$EXE" -test.run '^$' -test.bench BenchmarkFrame -test.count $COUNT -test.benchmem | tee "$OUT/frames.txt"
	benchstat "$OUT/frames.txt" >"$OUT/benchstat_frames.txt"

	step "Tournoi : parties en parallèle et arrêt précoce"
	"$OUT/bin/v4.test$EXE" -test.run '^$' -test.bench BenchmarkTournament -test.benchtime 1x -test.count 3 | tee "$OUT/tournament.txt"
	"$OUT/bin/v4.test$EXE" -test.run TestTournamentCancelLatency -test.v | tee "$OUT/cancel.txt"

	step "hyperfine : $TICKS ticks par exécution, $WARMUP chauffes, $RUNS passes"
	hyperfine -N --warmup $WARMUP --runs $RUNS --export-json "$OUT/hyperfine.json" --export-markdown "$OUT/hyperfine.md" \
		-n "V0 naïve" "$OUT/bin/v0$EXE -ticks $TICKS -workers 1" \
		-n "V1 grille" "$OUT/bin/v1$EXE -ticks $TICKS -workers 1" \
		-n "V2 zéro alloc" "$OUT/bin/v2$EXE -ticks $TICKS -workers 1" \
		-n "V3 64 octets" "$OUT/bin/v3$EXE -ticks $TICKS -workers 1" \
		-n "V4 sincos" "$OUT/bin/v4$EXE -ticks $TICKS -workers 1" \
		-n "V5 pool $WORKERS workers" "$OUT/bin/v4$EXE -ticks $TICKS -workers $WORKERS"
	go run ./tools/hfstats "$OUT/hyperfine.json" | tee "$OUT/hyperfine_stats.md"

	step "Profils CPU et mémoire en début de partie, flamegraphs"
	profiles

	step "Serveur réel sous charge (8 clients) : trames JSON puis binaires"
	load json
	load bin

	complements
}

# complements : mesures croisées du rapport. Les variantes instrumentées
# (compteurs, contexte sans remplissage) sont compilées dans une copie
# jetable du dépôt : le code mesuré par l'étape cpu n'est pas modifié.
complements() {
	local X=$OUT/extra tmp
	mkdir -p "$X"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' RETURN

	step "sincos : A/B croisé (naive_trig contre défaut), 4 × 5 passes alternées"
	go test -c -tags naive_trig -o "$tmp/trig_old.test$EXE" .
	go test -c -o "$tmp/trig_new.test$EXE" .
	rm -f "$X/trig_old.txt" "$X/trig_new.txt"
	for r in 1 2 3 4; do
		"$tmp/trig_old.test$EXE" -test.run '^$' -test.bench BenchmarkUpdate -test.benchtime 30x -test.count 5 -workers 1 >>"$X/trig_old.txt"
		"$tmp/trig_new.test$EXE" -test.run '^$' -test.bench BenchmarkUpdate -test.benchtime 30x -test.count 5 -workers 1 >>"$X/trig_new.txt"
	done
	(cd "$X" && benchstat trig_old.txt trig_new.txt) | tee "$X/benchstat_trig.txt"

	step "Alignement dans la version O(N²) : 88 octets contre 64 octets, 2 × 5 passes alternées"
	go test -c -tags naive_layout,naive_grid,naive_alloc,naive_trig -o "$tmp/on2_88.test$EXE" .
	go test -c -tags naive_grid,naive_alloc,naive_trig -o "$tmp/on2_64.test$EXE" .
	rm -f "$X/on2_88.txt" "$X/on2_64.txt"
	for r in 1 2; do
		"$tmp/on2_88.test$EXE" -test.run '^$' -test.bench 'BenchmarkUpdate/Debut' -test.benchtime 30x -test.count 5 -workers 1 >>"$X/on2_88.txt"
		"$tmp/on2_64.test$EXE" -test.run '^$' -test.bench 'BenchmarkUpdate/Debut' -test.benchtime 30x -test.count 5 -workers 1 >>"$X/on2_64.txt"
	done
	(cd "$X" && benchstat on2_88.txt on2_64.txt) | tee "$X/benchstat_on2_layout.txt"

	step "V0 : calculs de distance par tick (copie instrumentée)"
	mkdir -p "$tmp/count"
	cp ./*.go go.mod go.sum index.html "$tmp/count/"
	rm -f "$tmp/count/store_test.go"
	sed -i 's/^import "math"$/import (\n\t"math"\n\t"sync\/atomic"\n)\n\nvar nearestScanned, rowScanned, nearestCalls, rowCalls atomic.Int64/' "$tmp/count/grid_naive.go"
	sed -i 's/^func (g \*SpatialGrid) RowItems(cy, cx0, cx1 int) \[\]int32 {$/&\n\trowCalls.Add(1)\n\trowScanned.Add(int64(len(g.Items)))/' "$tmp/count/grid_naive.go"
	sed -i 's/^func (g \*SpatialGrid) Nearest(.*{$/&\n\tnearestCalls.Add(1)\n\tnearestScanned.Add(int64(len(g.Items)))/' "$tmp/count/grid_naive.go"
	cat >"$tmp/count/count_test.go" <<'GO'
//go:build naive_grid

package main

import "testing"

func TestCountDistances(t *testing.T) {
	cfg := DefaultConfig(MasterSeed)
	cfg.Workers = 1
	w := NewWorldWith(cfg)
	const n = 30
	for i := 0; i < n; i++ {
		w.Update()
	}
	t.Logf("par tick (début de partie, %d ticks) : Nearest %d appels, %d candidats ; RowItems %d appels, %d candidats ; total %d distances",
		n, nearestCalls.Load()/n, nearestScanned.Load()/n, rowCalls.Load()/n, rowScanned.Load()/n,
		(nearestScanned.Load()+rowScanned.Load())/n)
}
GO
	(cd "$tmp/count" && go test -tags naive_layout,naive_grid,naive_alloc,naive_trig -run TestCountDistances -v . | grep 'par tick') | tee "$X/distances.txt"

	step "Faux partage : contexte de décision avec et sans remplissage de 64 octets"
	mkdir -p "$tmp/nopad"
	cp ./*.go go.mod go.sum index.html "$tmp/nopad/"
	rm -f "$tmp/nopad/store_test.go"
	sed -i '/^\t_ \[64\]byte$/d' "$tmp/nopad/workers.go"
	(cd "$tmp/nopad" && go test -c -o "$tmp/nopad.test$EXE" .)
	go test -c -o "$tmp/pad.test$EXE" .
	rm -f "$X/pad.txt" "$X/nopad.txt"
	for r in 1 2; do
		"$tmp/pad.test$EXE" -test.run '^$' -test.bench 'BenchmarkWorkers/w(04|08|12)' -test.benchtime 100x -test.count 5 >>"$X/pad.txt"
		"$tmp/nopad.test$EXE" -test.run '^$' -test.bench 'BenchmarkWorkers/w(04|08|12)' -test.benchtime 100x -test.count 5 >>"$X/nopad.txt"
	done
	(cd "$X" && benchstat pad.txt nopad.txt) | tee "$X/benchstat_falsesharing.txt"

	step "Bilan de partie après 300 ticks, par version (même partie ?)"
	{
		for v in $VERSIONS; do
			echo "$v : $("$OUT/bin/$v$EXE" -ticks 300 -workers 1)"
		done
		echo "v5 : $("$OUT/bin/v4$EXE" -ticks 300 -workers $WORKERS)"
	} | tee "$X/etat_300.txt"
}

sql() {
	need docker "Docker Desktop ou Docker Engine"
	docker info >/dev/null 2>&1 || {
		echo "Docker ne répond pas : démarrer Docker Desktop"
		exit 1
	}
	mkdir -p "$OUT/bin"
	step "PostgreSQL (docker compose)"
	docker compose up -d --wait
	export PRV_DB="$DB"
	go build -o "$OUT/bin/v4$EXE" .
	rows=$(docker compose exec -T postgres psql -U prv -d prv -tAc "SELECT count(*) FROM run_ticks" 2>/dev/null | tr -d '\r ' || echo 0)
	if [ "${rows:-0}" -lt 1000000 ]; then
		step "Remplissage : tournoi de 40 parties enregistré en base (~1,1 million de lignes)"
		"$OUT/bin/v4$EXE" -tournoi 40 -db "$PRV_DB" | tee "$OUT/sql_tournoi.txt"
	fi
	step "Aller-retour, EXPLAIN ANALYZE sans puis avec index"
	go test -run 'TestStoreRoundTrip|TestExplainCurve' -v . | tee "$OUT/sql_explain.txt"
	step "Écriture : INSERT ligne à ligne contre COPY"
	go test -run '^$' -bench BenchmarkSaveTelemetry -benchtime 1x -count 3 . | tee "$OUT/sql_insert.txt"
	step "Lecture : sans index, index (run_id, tick), cache LRU"
	go test -run '^$' -bench BenchmarkCurve -benchmem -count $COUNT . | tee "$OUT/sql_curve.txt"
	benchstat "$OUT/sql_curve.txt" >"$OUT/benchstat_sql_curve.txt"
	docker compose stop
}

# profils : seulement les profils, les flamegraphs et la charge réseau, à
# partir des binaires déjà compilés par l'étape cpu.
profils_seuls() {
	mkdir -p "$OUT/prof"
	[ -x "$OUT/bin/v0.test$EXE" ] || {
		echo "lancer d'abord ./run_benchmarks.sh cpu"
		exit 1
	}
	step "Profils CPU et mémoire, flamegraphs"
	profiles
	step "Serveur réel sous charge (8 clients) : trames JSON puis binaires"
	load json
	load bin
}

case "${1:-tout}" in
cpu) cpu ;;
sql) sql ;;
profils) profils_seuls ;;
flammes) flammes ;;
complements)
	[ -x "$OUT/bin/v4$EXE" ] || {
		echo "lancer d'abord ./run_benchmarks.sh cpu"
		exit 1
	}
	complements
	;;
tout)
	cpu
	sql
	;;
*)
	echo "usage : $0 [cpu|sql|profils|flammes|complements|tout]"
	exit 2
	;;
esac
step "Terminé : résultats dans $OUT/"
