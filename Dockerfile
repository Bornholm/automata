# syntax=docker/dockerfile:1.7
#
# Image multi-stage pour Automata (PLAN.md Phase 22).
#
# Les trois bibliotheques maison
# (go-courier, genai, amoxtli) sont desormais publiees sur le module proxy et
# resolues comme n'importe quelle dependance. Le build ne demande plus ni
# contexte supplementaire ni disposition particuliere des depots sur le
# disque : `docker build .` suffit.

FROM golang:1.26-bookworm AS build

WORKDIR /src/automata

COPY . .

# CGO_ENABLED=0 : le driver SQLite (ncruces/go-sqlite3) est pur Go/WASM,
# sans cgo (deja verifie par la suite de tests de ce depot, qui passe sans
# cgo). Le binaire produit est donc statique, compatible avec l'image finale
# distroless "static" (sans libc ni interprete dynamique).
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/automata ./cmd/automata

# Plugins : chaque plugins/<nom>/ est un module Go a part entiere, compile
# ici vers /out/plugins. Sans cette etape, plugins.dir pointe sur un
# repertoire vide en production et aucun plugin ne se charge. Meme
# CGO_ENABLED=0 que le binaire principal : le gestionnaire de plugins lance
# ces binaires depuis l'image distroless, qui n'a ni libc ni interpreteur
# dynamique.
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    mkdir -p /out/plugins; \
    for dir in plugins/*/; do \
        [ -f "$dir/go.mod" ] || continue; \
        name=$(basename "$dir"); \
        echo "building plugin $name"; \
        (cd "$dir" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "/out/plugins/$name" .); \
    done

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

# Binaires de plugins. Le repertoire est en lecture seule dans l'image : le
# gestionnaire ne fait que lister et executer, il n'y ecrit jamais (configs
# et secrets des plugins vivent en base, cotes hote).
COPY --from=build /out/plugins /plugins

USER nonroot:nonroot

# /data (lecture-ecriture), /config et /prompts (lecture seule cote hote,
# voir compose.yaml et docs/deployment.md) : declares ici a titre
# documentaire, le montage effectif se fait via compose.yaml ou
# `docker run -v`.
VOLUME ["/data", "/config", "/prompts"]

# Sonde de santé : distroless "static" n'a ni shell ni client HTTP, la forme
# habituelle (`CMD curl -f ...`) est donc impossible. Le binaire applicatif
# fournit lui-même la sonde via sa sous-commande `healthcheck`, qui interroge
# GET /healthz/ready (internal/observability, Phase 20) et se contente d'un
# code de sortie 0/1 — la forme exec ci-dessous n'a besoin d'aucun shell.
#
# Prérequis : observability.enabled doit valoir true dans la configuration, et
# observability.addr correspondre à l'adresse sondée. Pour une autre adresse,
# ajouter `-addr <hôte:port>` à la commande. Voir docs/deployment.md §7.
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/automata", "healthcheck"]

ENTRYPOINT ["/usr/local/bin/automata"]
CMD ["-config", "/config/config.yaml"]
