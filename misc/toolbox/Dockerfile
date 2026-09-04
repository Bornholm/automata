# Image "toolbox" : le serveur MCP HTTP de LeaSH, accompagné des outils que
# le plugin workspace d'Automata est autorisé à lancer (ffmpeg, imagemagick,
# LibreOffice, tesseract, yt-dlp...) et des deux policies qui les encadrent.
#
# Elle est construite ICI, dans le dépôt d'Automata, et publiée sur
# ghcr.io/bornholm/leash-toolbox par la CI. LeaSH est un serveur d'exécution
# générique ; ce qu'on met dans le bac à sable et ce qu'on y autorise est
# un choix d'Automata, donc ces fichiers vivent avec lui. Le serveur, lui,
# est installé depuis le module Go publié, épinglé par LEASH_VERSION.
#
# Base Debian slim plutôt qu'Alpine : ffmpeg et imagemagick y sont packagés
# avec le jeu de codecs attendu, et les binaires sont liés à la glibc.
#
# Utilisateur non root : bubblewrap fonctionne sous Docker sans privilèges
# tant que le conteneur est lancé avec
#   --security-opt seccomp=unconfined
#   --security-opt apparmor=unconfined
#   --security-opt systempaths=unconfined
# Les deux premières lèvent l'interdiction de créer un user namespace et de
# repropager la racine du mount namespace ; la troisième démasque /proc,
# sans quoi bwrap ne peut pas y monter son propre procfs. Si l'exploitant
# ne peut pas accorder ces options, basculer le serveur sur
# LEASH_SANDBOX_BACKEND=chroot, qui exige alors root dans le conteneur et
# fait du conteneur lui-même la frontière de sécurité.

FROM golang:1.26-bookworm AS builder

# LEASH_VERSION est une version taguée ou un commit du module
# github.com/bornholm/leash. Monter de version = changer cet ARG, et le
# diff dit ce qui a bougé.
ARG LEASH_VERSION=v0.14.0

RUN CGO_ENABLED=0 GOFLAGS=-ldflags=-s\ -w \
    GOBIN=/out go install github.com/bornholm/leash/cmd/server@${LEASH_VERSION} \
    && mv /out/server /out/leash-server

# Deno : runtime JavaScript exigé par yt-dlp depuis que YouTube signe ses
# URLs de flux par un défi JS (« nsig »). Sans lui, l'extraction échoue sur
# « This video is not available » — y compris pour une vidéo parfaitement
# publique — et aucun `player_client` de repli ne s'en sort : tous
# retournent « Requested format is not available ».
#
# Deno plutôt que Node ou Bun : c'est le runtime activé par défaut par
# yt-dlp, et le seul à exécuter le script SANS aucun accès au système de
# fichiers ni au réseau. Le JS vient de YouTube, donc d'un tiers : cette
# restriction est le cœur du choix.
#
# Étape séparée pour que curl et unzip servent au téléchargement sans
# jamais entrer dans l'image finale — celle-ci ne doit embarquer aucun
# outil réseau (cf. policies/toolbox.yaml, sans réseau).
#
# Version et empreinte épinglées : une montée de version se relit dans le
# diff. Architecture x86_64 en dur, comme la cible de déploiement.
FROM debian:bookworm-slim AS deno

ARG DENO_VERSION=v2.9.6
ARG DENO_SHA256=394f07f4da2bebe6ce6f1e7ce0fa16429b29b08c35e3fac3fe25972676dff4b2

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl unzip \
    && curl -sSfL -o /tmp/deno.zip \
         "https://github.com/denoland/deno/releases/download/${DENO_VERSION}/deno-x86_64-unknown-linux-gnu.zip" \
    && echo "${DENO_SHA256}  /tmp/deno.zip" | sha256sum -c - \
    && unzip -q /tmp/deno.zip -d /usr/local/bin \
    && chmod 0755 /usr/local/bin/deno

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      bubblewrap \
      ffmpeg \
      imagemagick \
      ca-certificates \
      coreutils \
      file \
      pandoc \
      libreoffice-writer \
      libreoffice-calc \
      libreoffice-impress \
      poppler-utils \
      qpdf \
      # OCR : sans lui, un document scanné n'est qu'une image dans un PDF,
      # ni sélectionnable ni interrogeable. ocrmypdf tire python3.
      tesseract-ocr \
      tesseract-ocr-fra \
      tesseract-ocr-eng \
      ocrmypdf \
      # Métadonnées : une photo de téléphone embarque la position GPS et
      # l'identifiant de l'appareil. Les effacer avant partage est un geste
      # de confidentialité courant.
      libimage-exiftool-perl \
      jq \
      qrencode \
      zbar-tools \
      fonts-liberation \
      fonts-dejavu \
      # Sans Noto, un PDF contenant du chinois, de l'arabe ou un emoji sort
      # avec des carrés vides.
      fonts-noto-core \
      fonts-noto-color-emoji \
      # Téléchargement de vidéos publiques : utilisable UNIQUEMENT depuis
      # la policy « fetch » (la seule à ouvrir le réseau), et seulement à
      # travers les scripts fetch-video et fetch-subtitles — `yt-dlp`
      # n'est dans aucune
      # allowlist. Installé par pip, la version Debian étant périmée avant
      # même la sortie de l'image.
      python3-pip \
      zip \
      unzip \
      sed \
      grep \
      gawk \
      diffutils \
    && rm -rf /var/lib/apt/lists/*

# LibreOffice refuse de démarrer sans profil utilisateur inscriptible et le
# recrée à chaque exécution du bac à sable. Ce script lui en impose un dans
# /tmp (tmpfs) : sans lui, le profil atterrirait dans le workspace du
# membre, où il polluerait ses fichiers et consommerait son quota. Il fixe
# aussi le répertoire de sortie, faute de quoi soffice écrit à côté du
# fichier d'entrée sans que l'agent puisse le prévoir.
COPY office-convert /usr/local/bin/office-convert
RUN chmod 0755 /usr/local/bin/office-convert

# yt-dlp suit de très près les changements des plateformes : la version
# empaquetée par Debian est déjà en retard à la construction de l'image.
# --break-system-packages : c'est une image dédiée, il n'y a pas
# d'environnement Python système à préserver.
#
# L'extra « default » embarque yt-dlp-ejs, le script qui résout le défi de
# signature de YouTube. Sans lui, yt-dlp veut le TÉLÉCHARGER à chaque
# exécution (--remote-components ejs:github) : une dépendance réseau de
# plus, et un script tiers récupéré à chaud plutôt que figé dans l'image.
#
# L'extra « curl-cffi » apporte l'imitation de navigateur (empreinte TLS).
# TikTok l'EXIGE : sans elle, chaque extraction s'arrête sur « Unexpected
# response from webpage request », précédée d'un avertissement sur
# l'impersonation indisponible — un message qui n'évoque en rien la cause
# et envoie chercher un problème de droits ou de vidéo privée. Toutes les
# vidéos TikTok échouaient, pas seulement certaines.
#
# curl_cffi est une bibliothèque Python : elle n'ajoute aucun binaire, et
# n'entre donc dans aucune allowlist. Comme python3 et deno, elle n'est
# atteignable qu'en dépendance de yt-dlp, lui-même invocable seulement à
# travers ses wrappers.
RUN pip3 install --no-cache-dir --break-system-packages -U "yt-dlp[default,curl-cffi]"

# Le runtime JS que yt-dlp cherche pour résoudre le défi de signature de
# YouTube. Comme yt-dlp lui-même, `deno` n'est dans AUCUNE allowlist : il
# n'est jamais invocable depuis un script d'agent, seulement en
# sous-processus de yt-dlp — même règle que python3, qui ne sert qu'à
# ocrmypdf.
COPY --from=deno /usr/local/bin/deno /usr/local/bin/deno

# Les deux wrappers de yt-dlp (pas de --exec, pas de playlist, schéma et
# hôte contrôlés) : fetch-video rapporte la vidéo sous des plafonds de
# taille et de durée, fetch-subtitles rapporte les seuls sous-titres sans
# jamais télécharger la vidéo. Ce sont les seuls binaires autorisés par
# policies/fetch.yaml, la seule policy à ouvrir le réseau.
COPY fetch-video /usr/local/bin/fetch-video
COPY fetch-subtitles /usr/local/bin/fetch-subtitles
RUN chmod 0755 /usr/local/bin/fetch-video /usr/local/bin/fetch-subtitles

RUN useradd --create-home --uid 1000 --shell /usr/sbin/nologin leash

WORKDIR /app

COPY --from=builder /out/leash-server /app/leash-server
COPY policies/ /app/policies/

RUN mkdir -p /var/lib/leash/workspaces && chown leash:leash /var/lib/leash/workspaces

VOLUME ["/var/lib/leash/workspaces"]

EXPOSE 8443

ENV LEASH_MCP_LISTEN_ADDR=0.0.0.0:8443 \
    LEASH_WORKSPACE_ROOT=/var/lib/leash/workspaces \
    LEASH_SANDBOX_BACKEND=bwrap

USER leash

ENTRYPOINT ["/app/leash-server"]
