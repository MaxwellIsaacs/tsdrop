PREFIX ?= $(HOME)/.local
BIN    := tsdrop

build:
	go build -o $(BIN) .

# Static binary for servers without a Go toolchain (e.g. the relay box).
static:
	CGO_ENABLED=0 go build -o $(BIN) .

install: build
	install -Dm755 $(BIN) $(PREFIX)/bin/$(BIN)
	install -Dm644 assets/dev.tsdrop.gui.desktop $(PREFIX)/share/applications/dev.tsdrop.gui.desktop
	install -Dm644 assets/tsdrop.svg $(PREFIX)/share/icons/hicolor/scalable/apps/tsdrop.svg
	install -Dm644 assets/tsdrop-recv.service $(HOME)/.config/systemd/user/tsdrop-recv.service
	@echo "now: systemctl --user enable --now tsdrop-recv"

install-relay: static
	install -Dm755 $(BIN) $(PREFIX)/bin/$(BIN)
	install -Dm644 assets/tsdrop-relay.service $(HOME)/.config/systemd/user/tsdrop-recv.service
	@echo "edit FORWARD / HTTPS_URL in ~/.config/systemd/user/tsdrop-recv.service, then:"
	@echo "  systemctl --user daemon-reload && systemctl --user enable --now tsdrop-recv"

test:
	go vet ./...

.PHONY: build static install install-relay test
