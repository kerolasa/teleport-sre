GOPKGURL := github.com/kerolasa/teleport-sre
VERSION := $(shell git rev-list --count HEAD)-$(shell git rev-parse --short HEAD)
REPOPATH := $(shell git rev-parse --show-toplevel)
GOLDFLAGS := -X "$(GOPKGURL)/internal.Version=$(VERSION)" -X "$(GOPKGURL)/internal.Repopath=$(REPOPATH)"

# Avoid target reordering causing default target to change
.DEFAULT_GOAL := help

# The binaries go into this directory, because a binary of the same name as
# the source directory it is built from cannot sit next to that directory.
BINDIR := bin

# A binary is built from these, so it is built again when one of them is newer
# than the binary itself.  A binary is not built out of a test.
SOURCES := $(shell find . -type f -name '*.go' -not -name '*_test.go') go.mod go.sum

# The keys and the certificates go into this directory, since they are of no
# use to anyone but the workstation that made them.
CERTDIR := certs

# help is the default target
help:
	@echo "make controller-runtime  build $(BINDIR)/controller-runtime"
	@echo "make client-go           build $(BINDIR)/client-go"
	@echo "make build               build all binaries"
	@echo "make certs               create the tls keys and certificates in $(CERTDIR)"
	@echo "make run                 build and run against ./sample-config.toml"
	@echo "make gofmt               rewrite all go files with gofmt -s"
	@echo "make test                run go test, go vet, gofmt check and golangci-lint"
	@echo "make clean               remove all untracked and ignored files"

# controller-runtime and client-go are the binaries of the two main packages,
# under the names they are asked for by.
controller-runtime: $(BINDIR)/controller-runtime
client-go: $(BINDIR)/client-go

$(BINDIR)/controller-runtime: $(SOURCES) | $(BINDIR)
	go build -ldflags "$(GOLDFLAGS)" -o $@ ./controller-runtime

$(BINDIR)/client-go: $(SOURCES) | $(BINDIR)
	go build -ldflags "$(GOLDFLAGS)" -o $@ ./client-go

# build is the common case
build: controller-runtime client-go

# The directory is a prerequisite in the order-only sense, so that a binary
# written into it does not make the others look out of date.
$(BINDIR):
	mkdir -p $(BINDIR)

# certs creates the certificate authority, and the server and the client
# certificates it signs, which the api server serves mTLS with.  It leaves an
# existing set in $(CERTDIR) alone.
certs:
	./build-tools/make-certs.sh $(CERTDIR)

# A phony target is one that is not really the name of a file; rather it is
# just a name for a recipe to be executed when you make an explicit request.
.PHONY: help build certs run gofmt test clean

run: certs controller-runtime
	./$(BINDIR)/controller-runtime --config ./sample-config.toml

gofmt:
	find . -name '*.go' -exec gofmt -s -w {} +

test:
	go test -race ./...
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
