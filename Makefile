GO ?= go
BIN := bin/claude-session-health
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOSEC := github.com/securego/gosec/v2/cmd/gosec@v2.29.0

.PHONY: build test cover fmt fmt-check vet lint sec check link unlink clean

build:
	$(GO) build -trimpath -o $(BIN) ./cmd/claude-session-health

test:
	$(GO) test -race ./...

cover:
	$(GO) test -race -cover ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

# vet also type-checks the Linux build so platform code stays isolated.
vet:
	$(GO) vet ./...
	GOOS=linux $(GO) vet ./...

lint:
	$(GO) run $(STATICCHECK) ./...

sec:
	$(GO) run $(GOSEC) -quiet ./...

check: fmt-check vet lint test build

link: build
	herdr plugin link "$(CURDIR)"

unlink:
	herdr plugin unlink claude-session-health

clean:
	rm -rf bin
