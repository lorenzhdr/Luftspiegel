.PHONY: all build doubletake doubletake-ctl doubletake-release doubletake-ctl-release manpages-release install install-man uninstall test clean luftspiegel luftspiegel-ctl gui-binaries

PREFIX ?= /usr/local
MANDIR ?= $(PREFIX)/share/man

all: doubletake doubletake-ctl

build: all

doubletake:
	go build -o bin/doubletake ./cmd/doubletake

doubletake-ctl:
	go build -o bin/doubletake-ctl ./cmd/doubletake-ctl

# --- Windows-Port: Sidecar-Binaries für die Electron-GUI -------------------
#
# Die GUI sucht die Sidecars unter bin/luftspiegel.exe bzw.
# bin/luftspiegel-ctl.exe (gui/main.js:resolveSidecarPath und
# gui/package.json:extraResources), das Upstream-Makefile baut aber
# doubletake(.exe). Bisher wurde von Hand umbenannt; diese Targets bauen
# direkt unter dem erwarteten Namen, damit der Installer-Build reproduzierbar
# ist.
#
# Unter Windows hängt Go das .exe selbst an. Wichtig: eine laufende GUI hält
# bin\luftspiegel.exe exklusiv geöffnet — vor dem Build beenden, sonst
# scheitert das Schreiben mit einem Zugriffsfehler.
gui-binaries: luftspiegel luftspiegel-ctl

luftspiegel:
	CGO_ENABLED=0 go build -o bin/luftspiegel ./cmd/doubletake

luftspiegel-ctl:
	CGO_ENABLED=0 go build -o bin/luftspiegel-ctl ./cmd/doubletake-ctl

doubletake-release:
	CGO_ENABLED=0 go build -ldflags='-s -w -extldflags=-static' -o doubletake ./cmd/doubletake

doubletake-ctl-release:
	CGO_ENABLED=0 go build -ldflags='-s -w -extldflags=-static' -o doubletake-ctl ./cmd/doubletake-ctl

manpages-release:
	tar -czf doubletake-manpages.tar.gz -C man man1

test:
	go test ./...

install: all install-man
	install -m 755 bin/doubletake $(PREFIX)/bin/
	install -m 755 bin/doubletake-ctl $(PREFIX)/bin/

install-man:
	install -d $(MANDIR)/man1
	install -m 644 man/man1/doubletake.1 $(MANDIR)/man1/
	install -m 644 man/man1/doubletake-ctl.1 $(MANDIR)/man1/

uninstall:
	rm -f $(PREFIX)/bin/doubletake
	rm -f $(PREFIX)/bin/doubletake-ctl
	rm -f $(MANDIR)/man1/doubletake.1
	rm -f $(MANDIR)/man1/doubletake-ctl.1

clean:
	rm -rf bin/
	go clean -testcache
