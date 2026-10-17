# Lumine — pure-Go Wayland compositor.
#
# The repository root has no .go files (code lives in cmd/ and internal/), so
# `go build` with no arguments fails here. Always target a package or ./...

GO      ?= go
BIN     := bin
LUMINE  := $(BIN)/lumine
CTL     := $(BIN)/luminectl

.DEFAULT_GOAL := build

.PHONY: all build run ctl test vet check fmt clean

all: build

## build: compile both binaries into ./bin
build: $(LUMINE) $(CTL)

$(LUMINE):
	$(GO) build -o $@ ./cmd/lumine

$(CTL):
	$(GO) build -o $@ ./cmd/luminectl

## run: build and run the compositor (set ARGS="..." to pass flags)
run: build
	./$(LUMINE) $(ARGS)

## ctl: build and run luminectl (e.g. make ctl ARGS="status")
ctl: build
	./$(CTL) $(ARGS)

## test: run the test suite
test:
	$(GO) test ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## check: verify the no-CGO build, vet and tests (CI target)
check:
	CGO_ENABLED=0 $(GO) build ./...
	CGO_ENABLED=0 $(GO) vet ./...
	CGO_ENABLED=0 $(GO) test ./...

## fmt: format all Go sources
fmt:
	$(GO) fmt ./...

## clean: remove built binaries
clean:
	$(RM) -r $(BIN)
