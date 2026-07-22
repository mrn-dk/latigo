GUEST_WASM ?= latigo.wasm
GOAL ?= Explore the workspace and report what you find.

.PHONY: all guest host test conformance run replay clean fmt vet

all: guest host

## guest: build the harness to WebAssembly (wasip1)
guest:
	GOOS=wasip1 GOARCH=wasm go build -o $(GUEST_WASM) ./cmd/latigo-guest

## host: build the reference local host CLI
host:
	go build -o latigo-local ./cmd/latigo-local

## test: run the full test suite (includes a real wasm run + replay)
test:
	go test ./...

## conformance: run just the host conformance suite
conformance:
	go test ./host/ -run TestConformance -v

## run: build the guest and run it with the reference host (mock LLM by default)
run: guest host
	./latigo-local -wasm $(GUEST_WASM) "$(GOAL)"

## replay: reconstruct the last run from its event log
replay: guest host
	./latigo-local -wasm $(GUEST_WASM) -replay

fmt:
	gofmt -w .

vet:
	go vet ./...

clean:
	rm -f $(GUEST_WASM) latigo-local latigo.events.jsonl
	rm -rf workspace
