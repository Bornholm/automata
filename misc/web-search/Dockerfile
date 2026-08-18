# Moteur de recherche web pour l'agent : SearXNG et son serveur MCP dans un
# seul conteneur.
#
# Pourquoi les réunir : le serveur MCP est le seul client de SearXNG. Les
# séparer obligerait à exposer l'API JSON de SearXNG sur le réseau, alors
# qu'ici elle ne quitte jamais la boucle locale du conteneur. Une seule
# surface est publiée, le point d'entrée MCP, et elle demande un jeton.
#
# Voir README.md pour la construction et le branchement sur mcp_servers.

ARG MCP_SEARXNG_VERSION=1.15.0
ARG SEARXNG_IMAGE=searxng/searxng:latest

FROM node:22-bookworm-slim AS mcp
ARG MCP_SEARXNG_VERSION
RUN npm install --global --omit=dev "mcp-searxng@${MCP_SEARXNG_VERSION}"

FROM ${SEARXNG_IMAGE} AS final

# L'image SearXNG n'a pas de gestionnaire de paquets : Node arrive par copie
# depuis l'étage de construction. Les deux images sont sur glibc, et
# libstdc++/libgcc sont déjà présentes ici (dépendances de granian).
COPY --from=mcp /usr/local/bin/node /usr/local/bin/node
COPY --from=mcp /usr/local/lib/node_modules/mcp-searxng /usr/local/lib/node_modules/mcp-searxng

# Superposition minimale sur les réglages par défaut de SearXNG. Le format
# json y est indispensable : c'est la voie qu'emprunte le serveur MCP.
COPY searxng-settings.yml /etc/searxng/settings.yml
COPY entrypoint.sh /usr/local/bin/web-search-entrypoint.sh

# SearXNG n'écoute que sur la boucle locale : rien d'autre que le serveur MCP,
# dans ce même conteneur, n'a de raison de l'interroger.
ENV GRANIAN_HOST=127.0.0.1 \
    GRANIAN_PORT=8080 \
    SEARXNG_URL=http://127.0.0.1:8080

# Transport HTTP du serveur MCP. 0.0.0.0 est nécessaire pour que la
# publication de port de Docker atteigne le processus ; c'est la publication
# elle-même qu'il faut restreindre à 127.0.0.1 côté hôte (voir README).
ENV MCP_HTTP_PORT=3000 \
    MCP_HTTP_HOST=0.0.0.0 \
    MCP_HTTP_HARDEN=true \
    MCP_HTTP_ALLOWED_ORIGINS=http://127.0.0.1 \
    SEARXNG_DEFAULT_LANGUAGE=fr \
    SEARXNG_MAX_RESULTS=10 \
    SEARXNG_MAX_RESULT_CHARS=500 \
    URL_READ_MAX_CHARS=8000

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/web-search-entrypoint.sh"]
