BIN      := mantel-cast
VERSION  ?= $(shell date +%Y.%m.%d)
LDFLAGS  := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 linux/arm darwin/arm64 darwin/amd64 windows/amd64

.PHONY: build dist test clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) .

# Static binaries for a NAS, Raspberry Pi, Mac or Windows box.
dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "-> dist/$(BIN)-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o dist/$(BIN)-$$os-$$arch$$ext . || exit 1; \
	done

# Same checks as CI (gofmt, vet, race detector).
test:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go test -race ./...

clean:
	rm -rf $(BIN) dist
