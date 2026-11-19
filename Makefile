.PHONY: build install uninstall install-system uninstall-system test cover vet integration clean

GOCOVMERGE := $(shell command -v gocovmerge 2>/dev/null || echo $(HOME)/go/bin/gocovmerge)

PREFIX ?= /usr/local
BINDIR := $(PREFIX)/bin

build:
	go build -o traytail .

install: build
	install -Dm755 traytail $(DESTDIR)$(BINDIR)/traytail
	# fwmark bypass (portal probes past an active tunnel) needs CAP_NET_ADMIN; skip silently when not root
	-if [ -z "$(DESTDIR)" ] && command -v setcap >/dev/null 2>&1; then \
		setcap cap_net_admin+ep $(DESTDIR)$(BINDIR)/traytail 2>/dev/null && echo "setcap: portal probes can bypass the tunnel" || true; \
	fi

uninstall:
	rm -f $(DESTDIR)$(BINDIR)/traytail

# install-system: binary + systemd template unit for the invoking user.
# Usage: sudo make install-system USER=$USER
# Gives the tray CAP_NET_ADMIN (portal probes bypass the tunnel) and
# journald logging (journalctl -u traytail@USER -f).
install-system: install
	@if [ -z "$(USER)" ]; then echo "usage: sudo make install-system USER=<username>" >&2; exit 1; fi
	sed -e 's|/usr/local/bin/traytail|$(BINDIR)/traytail|' dist/traytail@.service > $(DESTDIR)/etc/systemd/system/traytail@.service
	systemctl daemon-reload
	systemctl enable traytail@$(USER)
	systemctl restart traytail@$(USER) # load the just-installed binary into the running service

uninstall-system:
	-systemctl disable --now traytail@$(USER)
	rm -f $(DESTDIR)/etc/systemd/system/traytail@.service
	-systemctl daemon-reload

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