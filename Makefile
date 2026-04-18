APP := vocis
VERSION := $(shell git describe --tags --always)
LDFLAGS := -X main.version=$(VERSION)

EXTENSION_UUID := vocis@duboisf.github.io
EXTENSION_SRC  := extensions/vocis-gnome
EXTENSION_DEST := $(HOME)/.local/share/gnome-shell/extensions/$(EXTENSION_UUID)

.PHONY: build test fmt tidy install-extension uninstall-extension enable-extension gnome-shell-nested dev

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
install-extension: install
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

# Launch a nested gnome-shell in a window so extension edits can be tested
# without logging out of the outer session. The nested shell reads extensions
# from the same ~/.local/share path, so `make install-extension` + this target
# gives a fast iteration loop:
#   terminal 1: make install-extension && make gnome-shell-nested
#   terminal 2: DISPLAY=... gnome-extensions enable $(EXTENSION_UUID)
#
# MUTTER_DEBUG_DUMMY_MODE_SPECS sets the nested window size; override at will.
# To exit the nested shell: close its window.
gnome-shell-nested:
	MUTTER_DEBUG_DUMMY_MODE_SPECS=1280x800 \
		dbus-run-session -- gnome-shell --nested --wayland

# One-shot dev loop: builds + installs the extension, launches the nested
# gnome-shell, enables the extension inside it, and starts vocis serve in the
# same dbus-run-session so they share a session bus. Closing the nested
# shell window or hitting Ctrl-C exits both.
#
# Crucially we point XDG_CONFIG_HOME at a fresh temp dir so the nested
# session has its OWN dconf — without this, `enabled-extensions` is shared
# with the outer session and disabling the extension in one disables it in
# both. With a separate dconf you can `gnome-extensions disable
# vocis@duboisf.github.io` in your normal outer terminal to drop the
# outer's accelerator grab (so the hotkey reaches nested) without
# affecting the nested instance.
#
# VOCIS_CONFIG is pinned to the user's real config since the temp
# XDG_CONFIG_HOME would otherwise hide it from vocis.
#
# What works in this setup:
#   - Hotkey events from the nested shell's accelerator → vocis (via D-Bus)
#   - All extension methods exposed by io.github.duboisf.Vocis.Hotkey
#   - Quick iteration on extension.js and Go code without logging out
#
# What does NOT work cleanly inside the nested session (don't be surprised):
#   - Pasting into apps in the OUTER session (target window IDs are nested)
#   - The X11 overlay may render on the outer display rather than the nested
#     one depending on DISPLAY/WAYLAND_DISPLAY plumbing.
# Use this for extension/D-Bus iteration, not for full dictation tests.
dev: install-extension
	@TMP_CONFIG=$$(mktemp -d -t vocis-dev-XXXXXX) && \
		echo "[dev] nested XDG_CONFIG_HOME=$$TMP_CONFIG (separate dconf)" && \
		XDG_CONFIG_HOME=$$TMP_CONFIG \
		VOCIS_CONFIG=$$HOME/.config/vocis/config.yaml \
		dbus-run-session -- bash -c '\
			MUTTER_DEBUG_DUMMY_MODE_SPECS=1280x800 \
				gnome-shell --nested --wayland & \
			SHELL_PID=$$!; \
			VOCIS_PID=""; \
			cleanup() { \
				[ -n "$$VOCIS_PID" ] && kill $$VOCIS_PID 2>/dev/null; \
				kill $$SHELL_PID 2>/dev/null; \
				wait 2>/dev/null; \
				rm -rf '"$$TMP_CONFIG"'; \
			}; \
			trap cleanup EXIT INT TERM; \
			echo "[dev] waiting for nested shell to register on the session bus..."; \
			for i in 1 2 3 4 5; do \
				if gnome-extensions list >/dev/null 2>&1; then break; fi; \
				sleep 1; \
			done; \
			echo "[dev] enabling $(EXTENSION_UUID) in nested dconf only"; \
			gnome-extensions enable $(EXTENSION_UUID) || echo "[dev] warning: enable failed"; \
			echo "[dev] tip: \`gnome-extensions disable $(EXTENSION_UUID)\` in your OUTER terminal to free the accelerator"; \
			echo "[dev] starting vocis serve"; \
			./bin/$(APP) serve & \
			VOCIS_PID=$$!; \
			wait -n $$SHELL_PID $$VOCIS_PID; \
			echo "[dev] one of {nested-shell, vocis} exited — tearing down both"; \
		'
