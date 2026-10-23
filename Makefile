.PHONY: build run smoke vet

build:
	go build -o bin/newgit ./cmd/newgit

run: build
	./bin/newgit -addr :8080 -data ./data

vet:
	go vet ./...

# End-to-end check with a real git client (needs git >= 2.38 and node >= 18)
smoke:
	./scripts/smoke.sh
