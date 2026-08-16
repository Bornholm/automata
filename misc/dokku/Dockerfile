# Image construite par Dokku, à partir du commit poussé par `make dokku-deploy`.
#
# Différence avec le Dockerfile de la racine : celui-ci n'attend aucun contexte
# de build supplémentaire. Les dépendances non publiées (go-courier, genai,
# amoxtli) sont déjà dans vendor/, ajouté au commit par la cible make. Dokku
# construit donc l'image sans rien connaître des répertoires frères du dépôt.
#
# Ce fichier est copié à la racine sous le nom "Dockerfile" dans le commit
# poussé : Dokku le trouve là où il le cherche, sans configuration serveur.

FROM golang:1.26-bookworm AS build

WORKDIR /src/automata

COPY . .

# -mod=vendor : ignore les directives "replace" et le réseau, tout est dans
# vendor/. CGO_ENABLED=0 : le driver SQLite est en Go pur compilé en WASM, le
# binaire produit est donc statique et tourne sur une image distroless static.
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath -ldflags="-s -w" -o /out/automata ./cmd/automata

FROM gcr.io/distroless/static-debian12:nonroot AS final

# Base de fuseaux horaires IANA : distroless "static" ne fournit pas
# /usr/share/zoneinfo, dont time.LoadLocation a besoin pour les expressions
# cron des tâches planifiées.
COPY --from=build /usr/local/go/lib/time/zoneinfo.zip /zoneinfo.zip
ENV ZONEINFO=/zoneinfo.zip

COPY --from=build /out/automata /usr/local/bin/automata

# Les prompts sont versionnés : ils font partie de l'image, contrairement à la
# configuration, qui porte des choix propres au déploiement et arrive par un
# volume monté (voir misc/dokku/README.md).
COPY --from=build /src/automata/prompts /prompts

USER nonroot:nonroot

# /data porte les bases SQLite, l'index de mémoire et la session WhatsApp.
# /config porte config.yaml. Les deux DOIVENT être montés depuis le stockage
# Dokku : un conteneur est reconstruit à chaque déploiement, et tout ce qui
# n'est pas sur un volume disparaît, y compris la liaison WhatsApp.
VOLUME ["/data", "/config"]

# Aucun HEALTHCHECK : Dokku pilote lui-même ses vérifications, et l'app ne
# publie aucun port (voir Procfile, process "worker" uniquement). La sonde
# reste disponible via `make dokku-healthcheck`.

ENTRYPOINT ["/usr/local/bin/automata"]
CMD ["-config", "/config/config.yaml"]
