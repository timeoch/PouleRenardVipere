# Raccourcis ; tout passe par run_benchmarks.sh (Git Bash sous Windows).
WORKERS ?= 4

.PHONY: bench bench-cpu bench-sql bench-rapide test run db

bench:            ## toutes les mesures du rapport (~40 min)
	WORKERS=$(WORKERS) ./run_benchmarks.sh tout

bench-cpu:        ## simulation, réseau, profils, hyperfine
	WORKERS=$(WORKERS) ./run_benchmarks.sh cpu

bench-sql:        ## PostgreSQL : COPY, EXPLAIN ANALYZE, cache LRU
	./run_benchmarks.sh sql

bench-rapide:     ## passes réduites, pour vérifier que tout tourne
	RAPIDE=1 WORKERS=$(WORKERS) ./run_benchmarks.sh tout

test:
	go test -short .

run:
	go run . -workers $(WORKERS) -pprof localhost:6060

db:
	docker compose up -d --wait
