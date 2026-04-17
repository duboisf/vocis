APP := vocis
VERSION := $(shell git describe --tags --always)
LDFLAGS := -X main.version=$(VERSION)

EXTENSION_UUID := vocis@duboisf.github.io
EXTENSION_SRC  := extensions/vocis-gnome
EXTENSION_DEST := $(HOME)/.local/share/gnome-shell/extensions/$(EXTENSION_UUID)

.PHONY: build test fmt tidy install-extension uninstall-extension enable-extension

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(APP) ./cmd/vocis

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/vocis

test:
	go test ./...

fmt:
	gofmt -w ./cmd ./internal

tidy:
	go mod tidy

# Copy the GNOME Shell extension into the user's local extensions directory.
# After the first install you must log out and back in so gnome-shell rescans;
# subsequent runs of this target overwrite the files but still need a relogin
# for code changes to take effect (gnome-shell does not hot-reload on Wayland).
install-extension:
	mkdir -p $(EXTENSION_DEST)
	cp $(EXTENSION_SRC)/metadata.json $(EXTENSION_SRC)/extension.js $(EXTENSION_DEST)/
	@echo
	@echo "Installed $(EXTENSION_UUID) to $(EXTENSION_DEST)"
	@echo "Next steps:"
	@echo "  1. Log out and log back in (gnome-shell rescans on session start)"
	@echo "  2. gnome-extensions enable $(EXTENSION_UUID)"
	@echo "  3. vocis doctor   # wayland-hk should report ok"

enable-extension:
	gnome-extensions enable $(EXTENSION_UUID)

uninstall-extension:
	rm -rf $(EXTENSION_DEST)
	@echo "Removed $(EXTENSION_DEST) (log out/in to clear from gnome-shell)"
