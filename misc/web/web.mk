# Outillage du serveur web d'administration et de profil (internal/web).
# Inclus automatiquement par le "include misc/**/*.mk" du Makefile racine.
#
# Les artefacts générés (fichiers *_templ.go et internal/web/assets/app.css)
# sont COMMITÉS : "go build" reste autonome, ni templ ni tailwind ne sont
# nécessaires pour compiler ou déployer. Ces cibles ne servent qu'au
# développement des vues.

TAILWIND_VERSION ?= v4.1.11
# templui : la v2 (choix acté) n'est pas encore installable — ses tags beta
# portent un module renommé sans cmd/templui. On épingle la v1.13.1 stable
# (même workflow init/add + Tailwind v4) ; migrer = monter cette version.
TEMPLUI_VERSION ?= v1.13.1

.PHONY: web-tools web-generate

# Installe l'outillage sous tools/ (même modèle que tools/modd/bin).
web-tools: tools/templ/bin/templ tools/templui/bin/templui tools/tailwind/bin/tailwindcss

tools/templ/bin/templ:
	mkdir -p tools/templ/bin
	GOBIN=$(PWD)/tools/templ/bin go install github.com/a-h/templ/cmd/templ@latest

tools/templui/bin/templui:
	mkdir -p tools/templui/bin
	GOBIN=$(PWD)/tools/templui/bin go install github.com/templui/templui/cmd/templui@$(TEMPLUI_VERSION)

tools/tailwind/bin/tailwindcss:
	mkdir -p tools/tailwind/bin
	curl -sSfL -o tools/tailwind/bin/tailwindcss \
		https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-linux-x64
	chmod +x tools/tailwind/bin/tailwindcss

# Régénère les vues (templ) et la feuille de style (tailwind).
web-generate: web-tools
	tools/templ/bin/templ generate ./internal/web/...
	tools/tailwind/bin/tailwindcss -i internal/web/tokens.css -o internal/web/assets/app.css --minify
