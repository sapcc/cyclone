PKG:=github.com/sapcc/cyclone
APP_NAME:=cyclone
PWD:=$(shell pwd)
UID:=$(shell id -u)
VERSION:=$(shell git describe --tags --always --dirty="-dev" | sed "s/^v//")
LDFLAGS:=-X $(PKG)/pkg.Version=$(VERSION) -w -s

export CGO_ENABLED:=0

build: fmt vet
	GOOS=linux go build -mod=vendor -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) ./cmd/$(APP_NAME)
	GOOS=darwin go build -mod=vendor -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME)_darwin ./cmd/$(APP_NAME)
	GOOS=windows go build -mod=vendor -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME).exe ./cmd/$(APP_NAME)

docker:
	docker pull golang:latest
	docker run -ti --rm -e GOCACHE=/tmp -v $(PWD):/$(APP_NAME) -u $(UID):$(UID) --workdir /$(APP_NAME) golang:latest make

fmt:
	gofmt -s -w cmd pkg

vet:
	go vet -mod=vendor ./cmd/... ./pkg/...

mod:
	go mod vendor
