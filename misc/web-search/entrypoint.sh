#!/bin/sh
# Lance SearXNG puis le serveur MCP dans le même conteneur, et lie leurs
# destins : si l'un s'arrête, le conteneur s'arrête. Sans cela, Docker verrait
# un conteneur en bonne santé alors que la recherche ne répond plus.
set -eu

mcp_cli=/usr/local/lib/node_modules/mcp-searxng/dist/cli.js

if [ -z "${SEARXNG_SECRET:-}" ]; then
    # SearXNG refuse de démarrer sans clé. Elle ne signe ici que des cookies de
    # préférences dont aucun client MCP ne se sert : une valeur éphémère
    # convient. Fixer SEARXNG_SECRET reste préférable en déploiement durable.
    SEARXNG_SECRET=$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')
    export SEARXNG_SECRET
    echo "web-search: SEARXNG_SECRET absent, clé éphémère générée pour ce démarrage"
fi

if [ "${MCP_HTTP_HARDEN:-}" = "true" ] && [ -z "${MCP_HTTP_AUTH_TOKEN:-}" ]; then
    # Échouer au démarrage plutôt que d'ouvrir un point d'entrée MCP sans
    # jeton : un serveur de recherche anonyme est un relais de sortie ouvert.
    echo "web-search: MCP_HTTP_AUTH_TOKEN requis quand MCP_HTTP_HARDEN=true" >&2
    exit 1
fi

/usr/local/searxng/entrypoint.sh &
searxng_pid=$!

# Le serveur MCP interroge SearXNG dès son premier appel d'outil, pas au
# démarrage : l'attente sert surtout à ce qu'un healthcheck immédiat ne tombe
# pas sur un moteur encore muet.
i=0
while [ "$i" -lt 60 ]; do
    if node -e "fetch('http://127.0.0.1:8080/healthz').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))" 2>/dev/null; then
        break
    fi
    kill -0 "$searxng_pid" 2>/dev/null || { echo "web-search: SearXNG s'est arrêté au démarrage" >&2; exit 1; }
    i=$((i + 1))
    sleep 1
done

node "$mcp_cli" &
mcp_pid=$!

terminate() {
    kill "$searxng_pid" "$mcp_pid" 2>/dev/null || true
    exit 0
}
trap terminate INT TERM

# dash ne connaît pas « wait -n » : on surveille les deux processus.
while kill -0 "$searxng_pid" 2>/dev/null && kill -0 "$mcp_pid" 2>/dev/null; do
    sleep 5
done

echo "web-search: un des deux processus s'est arrêté, arrêt du conteneur" >&2
kill "$searxng_pid" "$mcp_pid" 2>/dev/null || true
wait "$searxng_pid" "$mcp_pid" 2>/dev/null || true
exit 1
