VERSION ?= 0.3.2
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test build dist image ship-host ship

test:
	CGO_ENABLED=0 go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/agent-board ./cmd/agent-board

dist:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/agent-board-linux-amd64 ./cmd/agent-board
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o dist/agent-board-windows-amd64.exe ./cmd/agent-board

image:
	docker build -f deploy/Dockerfile --build-arg VERSION=$(VERSION) -t agent-board:$(VERSION) .

# SHIP_HOST is an ssh destination (user@host or a ~/.ssh/config alias) whose user can run sudo docker.
SHIP_HOST ?=

ship-host:
	@test -n "$(SHIP_HOST)" || { echo "set SHIP_HOST, for example: make ship VERSION=$(VERSION) SHIP_HOST=admin@docker-host"; exit 1; }

ship: ship-host image
	docker save agent-board:$(VERSION) | ssh $(SHIP_HOST) sudo docker load
