# Service : SearXNG et son serveur MCP, tous deux en boucle locale.
#
# Le serveur MCP est le seul client de SearXNG, et Automata le seul client
# du serveur MCP : rien de tout cela ne quitte le conteneur. Le jeton
# d'accès est engendré ici quand il n'est pas fourni — il n'a personne
# d'autre à convaincre que le processus voisin, et l'exporter suffit à ce
# que la configuration d'Automata (${INTERNET_SEARCH_MCP_TOKEN}) le
# retrouve.
#
# Pour désactiver la recherche web sur un déploiement : supprimer ce
# fichier de l'image, ou poser AUTOMATA_DISABLE_SEARXNG=1.

searxng_pid=""
mcp_pid=""

service_start() {
    if [ "${AUTOMATA_DISABLE_SEARXNG:-}" = "1" ]; then
        log "searxng désactivé (AUTOMATA_DISABLE_SEARXNG=1)"
        return 0
    fi

    if [ -z "${SEARXNG_SECRET:-}" ]; then
        # SearXNG refuse de démarrer sans clé. Elle ne signe que des
        # cookies de préférences dont aucun client MCP ne se sert : une
        # valeur éphémère convient.
        SEARXNG_SECRET=$(head -c 24 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')
        export SEARXNG_SECRET
    fi

    if [ -z "${INTERNET_SEARCH_MCP_TOKEN:-}" ]; then
        INTERNET_SEARCH_MCP_TOKEN=$(head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')
        export INTERNET_SEARCH_MCP_TOKEN
        log "searxng: jeton MCP engendré pour ce démarrage"
    fi
    # Le serveur MCP et Automata lisent le même jeton sous deux noms.
    MCP_HTTP_AUTH_TOKEN=$INTERNET_SEARCH_MCP_TOKEN
    export MCP_HTTP_AUTH_TOKEN

    # L'URL que la configuration d'Automata référence ; surchargeable pour
    # pointer un SearXNG externe sans toucher au fichier de configuration.
    : "${INTERNET_SEARCH_MCP_URL:=http://127.0.0.1:${MCP_HTTP_PORT:-3000}/mcp}"
    export INTERNET_SEARCH_MCP_URL

    /usr/local/searxng/entrypoint.sh &
    searxng_pid=$!

    # Attendre SearXNG avant de lancer le serveur MCP : celui-ci
    # interroge le moteur dès son premier appel d'outil, et un premier
    # appel qui échoue serait vu du modèle comme une recherche vide.
    i=0
    while [ "$i" -lt 60 ]; do
        if node -e "fetch('http://127.0.0.1:8080/healthz').then(r=>process.exit(r.ok?0:1),()=>process.exit(1))" 2>/dev/null; then
            break
        fi
        kill -0 "$searxng_pid" 2>/dev/null || { log "searxng: arrêté au démarrage"; return 1; }
        i=$((i + 1))
        sleep 1
    done

    node /usr/local/lib/node_modules/mcp-searxng/dist/cli.js &
    mcp_pid=$!

    # Le superviseur ne surveille qu'un PID par service : celui du
    # serveur MCP, dont dépend la capacité annoncée à l'agent. SearXNG
    # est surveillé par le service lui-même, via service_ready puis par
    # la mort du MCP qui suivrait.
    service_pid=$mcp_pid
    return 0
}

service_ready() {
    [ "${AUTOMATA_DISABLE_SEARXNG:-}" = "1" ] && return 0
    node -e "fetch('http://127.0.0.1:${MCP_HTTP_PORT:-3000}/mcp',{method:'POST'}).then(()=>process.exit(0),()=>process.exit(1))" 2>/dev/null
}
