# Tramite: common tasks. `just` lists them.

default:
    @just --list

# Tests and checks (what CI runs)
test:
    go vet ./...
    test -z "$(gofmt -l .)" || { gofmt -l .; echo "run: just fmt"; exit 1; }
    go test -race ./...

fmt:
    gofmt -w .

# Run locally (copy config.example.yaml to config.yaml first)
run config="config.yaml":
    go run ./cmd/tramite -config {{config}}

build:
    CGO_ENABLED=0 go build -trimpath -o tramite ./cmd/tramite

# Docker image as CI builds it
docker tag="tramite:dev":
    docker build --build-arg VERSION=dev -t {{tag}} .

# Tag a release and push it; CI builds the image, pushes it and deploys.
# channel: alpha | beta | stable; bump: major | minor | patch | X.Y.Z (optional)
release channel bump="":
    tools/release.sh {{channel}} {{bump}}

# Tag current main as-is (e.g. promote the tested beta to stable)
promote channel bump="":
    tools/release.sh --promote {{channel}} {{bump}}
