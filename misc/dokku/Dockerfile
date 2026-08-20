# syntax=docker/dockerfile:1.7
#
# Image de deploiement d'Automata : le binaire, ses plugins, et les
# services annexes dont l'agent depend (SearXNG et son serveur MCP).
#
# Pourquoi tout reunir dans une image plutot que composer plusieurs
# conteneurs : Dokku deploie une image par application, et ces services
# n'ont aucun autre client qu'Automata. Les separer obligerait a exposer
# l'API de SearXNG sur le reseau, alors qu'ici elle ne quitte jamais la
# boucle locale du conteneur.
#
# L'image finale part de SearXNG, qui apporte Python et granian ; Node y
# est copie pour le serveur MCP, et le binaire Automata est statique
# (CGO_ENABLED=0), donc indifferent a la distribution hote.

ARG MCP_SEARXNG_VERSION=1.15.0
ARG SEARXNG_IMAGE=searxng/searxng:latest

FROM golang:1.26-bookworm AS build

WORKDIR /src/automata

COPY . .

# CGO_ENABLED=0 : le driver SQLite (ncruces/go-sqlite3) est pur Go/WASM,
# sans cgo. Le binaire produit est statique.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/automata ./cmd/automata

# Plugins : chacun est un module Go autonome, compile en binaire separe et
# lance en sous-processus par l'hote (voir internal/plugin). Un repertoire
# sans go.mod est ignore.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    mkdir -p /out/plugins; \
    for dir in plugins/*/; do \
        [ -f "$dir/go.mod" ] || continue; \
        name=$(basename "$dir"); \
        echo "plugin $name"; \
        (cd "$dir" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "/out/plugins/$name" .); \
    done

FROM node:22-bookworm-slim AS mcp
ARG MCP_SEARXNG_VERSION
RUN npm install --global --omit=dev "mcp-searxng@${MCP_SEARXNG_VERSION}"

FROM ${SEARXNG_IMAGE} AS final

# L'image SearXNG n'a pas de gestionnaire de paquets : Node arrive par
# copie. Les deux images sont sur glibc, et libstdc++/libgcc sont deja
# presentes ici (dependances de granian).
COPY --from=mcp /usr/local/bin/node /usr/local/bin/node
COPY --from=mcp /usr/local/lib/node_modules/mcp-searxng /usr/local/lib/node_modules/mcp-searxng

# Superposition minimale sur les reglages par defaut de SearXNG. Le format
# json y est indispensable : c'est la voie qu'emprunte le serveur MCP.
COPY misc/web-search/searxng-settings.yml /etc/searxng/settings.yml

COPY --from=build /out/automata /usr/local/bin/automata
COPY --from=build /out/plugins /plugins

# Les prompts sont versionnes : ils font partie de l'image, contrairement
# a la configuration, qui decrit un deploiement et arrive par un volume.
COPY --from=build /src/automata/prompts /prompts

# Superviseur et services annexes. Ajouter un serveur MCP a l'image, c'est
# ajouter un fichier dans services.d — voir misc/dokku/README.md.
COPY misc/dokku/entrypoint.sh /usr/local/bin/automata-entrypoint.sh
COPY misc/dokku/services.d/ /etc/automata/services.d/
RUN chmod +x /usr/local/bin/automata-entrypoint.sh

# Base de fuseaux horaires IANA, necessaire a time.LoadLocation utilise par
# le scheduler. L'image SearXNG en fournit une ; on garde la variable vide
# pour laisser la bibliotheque standard lire /usr/share/zoneinfo.

# SearXNG n'ecoute que sur la boucle locale : rien d'autre que le serveur
# MCP, dans ce meme conteneur, n'a de raison de l'interroger.
ENV GRANIAN_HOST=127.0.0.1 \
    GRANIAN_PORT=8080 \
    SEARXNG_URL=http://127.0.0.1:8080

# Le serveur MCP n'est atteint que depuis ce conteneur : loopback, et un
# jeton tout de meme (voir services.d/10-searxng.sh).
ENV MCP_HTTP_PORT=3000 \
    MCP_HTTP_HOST=127.0.0.1 \
    MCP_HTTP_HARDEN=true \
    MCP_HTTP_ALLOWED_ORIGINS=http://127.0.0.1 \
    SEARXNG_DEFAULT_LANGUAGE=fr \
    SEARXNG_MAX_RESULTS=10 \
    SEARXNG_MAX_RESULT_CHARS=500 \
    URL_READ_MAX_CHARS=8000

# Port du serveur web d'Automata (administration, profils, webhook Stripe,
# retours OAuth). C'est le seul port publie ; web.addr de la configuration
# doit ecouter sur 0.0.0.0 pour que le proxy l'atteigne.
ENV AUTOMATA_HTTP_PORT=5000
EXPOSE 5000

# /data porte les bases SQLite, l'index de memoire et la session WhatsApp.
# /config porte config.yaml. Les deux DOIVENT etre montes depuis le
# stockage Dokku : un conteneur est reconstruit a chaque deploiement.
VOLUME ["/data", "/config"]

# L'image SearXNG tourne sous l'utilisateur searxng (UID 977) : c'est lui
# qui doit posseder les volumes.
USER searxng

ENTRYPOINT ["/usr/local/bin/automata-entrypoint.sh"]
CMD ["-config", "/config/config.yaml"]
