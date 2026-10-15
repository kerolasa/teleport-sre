GOPKGURL := github.com/kerolasa/teleport-sre
VERSION := $(shell git rev-list --count HEAD)-$(shell git rev-parse --short HEAD)
REPOPATH := $(shell git rev-parse --show-toplevel)
GOLDFLAGS := -X "$(GOPKGURL)/internal.Version=$(VERSION)" -X "$(GOPKGURL)/internal.Repopath=$(REPOPATH)"

# Avoid target reordering causing default target to change
.DEFAULT_GOAL := help

# The binaries go into this directory, because a binary of the same name as
# the source directory it is built from cannot sit next to that directory.
BINDIR := bin

# help is the default target
help:
	@echo "make controller-runtime  build $(BINDIR)/controller-runtime"
	@echo "make build               build all binaries"
	@echo "make run                 build and run against ./sample-config.toml"
	@echo "make gofmt               rewrite all go files with gofmt -s"
	@echo "make test                run go test, go vet, gofmt check and golangci-lint"
	@echo "make clean               remove all untracked and ignored files"

controller-runtime: $(BINDIR)
	go build -ldflags "$(GOLDFLAGS)" -o $(BINDIR)/controller-runtime ./controller-runtime

# build is the common case
build: controller-runtime

$(BINDIR):
	mkdir -p $(BINDIR)

# A phony target is one that is not really the name of a file; rather it is
# just a name for a recipe to be executed when you make an explicit request.
.PHONY: help build controller-runtime run gofmt test clean

run: controller-runtime
	./$(BINDIR)/controller-runtime --config ./sample-config.toml

gofmt:
	find . -name '*.go' -exec gofmt -s -w {} +

test:
	go test ./...
	go vet ./...
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "$$unformatted"; \
		echo "the files above are not gofmt clean, run make gofmt"; \
		exit 1; \
	fi
	golangci-lint run

clean:
	git clean -xdf
