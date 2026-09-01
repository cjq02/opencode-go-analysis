DB ?= usage.db
ADDR ?= :18080
WS ?= wrk_01KQE6ZT476376EYCQDQ0AMC28

.PHONY: usage stats site serve build vet clean

usage:
	go run ./cmd/usage -db $(DB)

stats:
	go run ./cmd/stats -db $(DB) -view month
	go run ./cmd/stats -db $(DB) -view model
	go run ./cmd/stats -db $(DB) -view month-model
	go run ./cmd/stats -db $(DB) -view cache
	go run ./cmd/stats -db $(DB) -view daily-model -month 2026-08
	go run ./cmd/stats -db $(DB) -view deepseek-peak -start 2026-08-17

site:
	go run ./cmd/site -db $(DB)

serve:
	go run ./cmd/server -db $(DB) -addr $(ADDR)

build:
	go vet ./...
	go build -o bin/usage ./cmd/usage
	go build -o bin/stats ./cmd/stats
	go build -o bin/site ./cmd/site
	go build -o bin/server ./cmd/server

vet:
	go vet ./...

clean:
	rm -rf bin/ docs/index.html
