.PHONY: build test cover vet integration clean

GOCOVMERGE := $(shell command -v gocovmerge 2>/dev/null || echo $(HOME)/go/bin/gocovmerge)

build:
	go build -o traytail .

vet:
	go vet ./...

test:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	-@dbus-run-session -- go test -tags dbus_integration -coverprofile=coverage.int.out -covermode=atomic ./... > /dev/null 2>&1
	@if [ -f coverage.int.out ]; then \
		if [ -x "$(GOCOVMERGE)" ]; then \
			"$(GOCOVMERGE)" coverage.out coverage.int.out > coverage.merged.out && mv coverage.merged.out coverage.out; \
		fi; \
		rm -f coverage.int.out; \
	fi
	go tool cover -func=coverage.out | tail -1

integration:
	dbus-run-session -- go test -tags dbus_integration -v -run TestIntegration .

clean:
	rm -f traytail coverage.out coverage.html