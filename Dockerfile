# syntax=docker/dockerfile:1.7
#
# Image multi-stage pour Automata (PLAN.md Phase 22).
#
# PROBLEME BLOQUANT — dependances locales non publiees
# ------------------------------------------------------
# go.mod redirige (directives "replace") github.com/bornholm/go-courier,
# github.com/bornholm/genai et github.com/bornholm/amoxtli vers des chemins
# relatifs freres du module ("../go-courier", "../genai", "../amoxtli") :
# aucune des trois bibliotheques n'a de version taguee publiee sur un module
# proxy correspondant a l'API reellement utilisee ici (voir go.mod, Phase 5,
# docs/integration-inventory.md). Un `docker build` classique, avec pour
# seul contexte ce depot, echouera donc a `go mod download`/`go build`.
#
# Solution retenue (temporaire, voir docs/deployment.md §1) : fournir les
# sources des trois depots comme CONTEXTES DE BUILD SUPPLEMENTAIRES via
# Docker Buildx (--build-context), et les copier ici a des chemins relatifs
# identiques a ceux attendus par les "replace" de go.mod, en freres de
# /src/automata. Voir docs/deployment.md pour la commande de build exacte.
#
# Le jour ou go-courier/genai/amoxtli publient une version taguee : retirer
# les "replace" de go.mod, supprimer les trois lignes "COPY --from=" ci
# dessous ainsi que les contextes de build additionnels, et ce Dockerfile
# redevient un Dockerfile Go multi-stage ordinaire.

FROM golang:1.26-bookworm AS build

WORKDIR /src/automata

# Depots locaux non publies, injectes comme contextes de build nommes
# (voir docs/deployment.md §3 pour la commande `docker buildx build
# --build-context gocourier=... --build-context genai=... --build-context
# amoxtli=...`). Places en freres de /src/automata pour correspondre aux
# "replace" relatifs de go.mod (../go-courier, ../genai, ../amoxtli).
COPY --from=gocourier . /src/go-courier
COPY --from=genai . /src/genai
COPY --from=amoxtli . /src/amoxtli

COPY . .

# CGO_ENABLED=0 : le driver SQLite (ncruces/go-sqlite3) est pur Go/WASM,
# sans cgo (deja verifie par la suite de tests de ce depot, qui passe sans
# cgo). Le binaire produit est donc statique, compatible avec l'image finale
# distroless "static" (sans libc ni interprete dynamique).
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/automata ./cmd/automata

# Image finale minimale : aucun shell, aucun gestionnaire de paquets,
# surface d'attaque reduite au strict binaire. Variante "nonroot" :
# utilisateur UID/GID 65532 deja non privilegie, pas besoin de creer un
# utilisateur applicatif dedie.
FROM gcr.io/distroless/static-debian12:nonroot AS final

# Base de fuseaux horaires IANA : distroless "static" ne fournit pas
# /usr/share/zoneinfo, necessaire a time.LoadLocation utilise par le
# scheduler (internal/scheduler, schedules[].schedule.timezone). On
# reutilise l'archive zoneinfo.zip de l'image de build Go et on pointe
# ZONEINFO dessus (comportement documente du paquet time de la bibliotheque
# standard), plutot que de modifier le code Go pour embarquer time/tzdata.
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip

COPY --from=build /out/automata /usr/local/bin/automata

USER nonroot:nonroot

# /data (lecture-ecriture), /config et /prompts (lecture seule cote hote,
# voir compose.yaml et docs/deployment.md) : declares ici a titre
# documentaire, le montage effectif se fait via compose.yaml ou
# `docker run -v`.
VOLUME ["/data", "/config", "/prompts"]

# Pas de HEALTHCHECK natif : distroless "static" n'a ni shell ni binaire de
# requete HTTP pour l'executer. Voir docs/deployment.md §5 : la sonde de
# sante applicative reste GET /healthz/ready (internal/observability,
# Phase 20), a interroger depuis l'exterieur du conteneur (reverse proxy,
# orchestrateur, `docker compose ps` avec le healthcheck defini au niveau
# compose si un outil HTTP y est disponible).

ENTRYPOINT ["/usr/local/bin/automata"]
CMD ["-config", "/config/config.yaml"]
