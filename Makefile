GOMOD ?= github.com/mrn-dk/latigo
WASM ?= latigo.wasm
BIN ?= latigo
GOAL ?= Explore the workspace and report what you find.

.PHONY: all build wasm run test fmt vet clean

all: build

## build: compile the native binary (the single-host dev / test path)
build:
	go build -o $(BIN) .

## wasm: compile to WebAssembly (WASI Preview 1; runs under a WASIX runtime).
## The native `build` target is the tested path; WASIX runtime behaviour under
## Wasmer is the host system's concern, not Latigo's.
wasm:
	GOOS=wasip1 GOARCH=wasm go build -o $(WASM) .

## run: run the harness natively against a local endpoint and workspace
run: build
	./$(BIN) "$(GOAL)"

## test: run the full test suite (real shell, mock endpoint, event log)
test:
	go test ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f $(BIN) $(WASM) latigo.events.jsonl
	rm -rf workspace
