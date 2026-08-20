#!/bin/sh
# Superviseur de l'image de déploiement.
#
# Automata a besoin, dans son conteneur, de services annexes : aujourd'hui
# SearXNG et son serveur MCP, demain d'autres serveurs MCP. Plutôt qu'un
# point d'entrée qui grossirait à chaque ajout, chaque service se déclare
# dans un fichier de /etc/automata/services.d — ajouter un service est
# alors un fichier de plus, jamais une modification d'ici.
#
# Contrat d'un fichier de service (voir services.d/10-searxng.sh) :
#   - il est sourcé, pas exécuté : il peut exporter des variables que le
#     processus principal lira (jeton MCP, URL interne…) ;
#   - il définit service_start(), qui lance son processus en arrière-plan
#     et publie son PID dans la variable service_pid ;
#   - il peut définir service_ready(), sondée jusqu'à SERVICE_READY_TIMEOUT
#     secondes avant de poursuivre ;
#   - il renonce en retournant non-zéro : le démarrage s'arrête là, plutôt
#     que de lancer Automata amputé d'une capacité qu'il croit avoir.
#
# Les destins sont liés : si un service meurt, le conteneur s'arrête. Sans
# cela, l'orchestrateur verrait un conteneur sain alors que la recherche,
# par exemple, ne répond plus.
set -eu

SERVICES_DIR=${SERVICES_DIR:-/etc/automata/services.d}
SERVICE_READY_TIMEOUT=${SERVICE_READY_TIMEOUT:-60}

pids=""

log() { echo "entrypoint: $*"; }

start_services() {
    [ -d "$SERVICES_DIR" ] || return 0

    for service in "$SERVICES_DIR"/*.sh; do
        [ -e "$service" ] || continue
        name=$(basename "$service" .sh)

        # Chaque service repart d'une table rase : un service qui oublie
        # de définir service_ready ne doit pas hériter de celle du
        # précédent.
        service_pid=""
        unset -f service_start service_ready 2>/dev/null || true

        log "chargement du service $name"
        # shellcheck disable=SC1090
        . "$service"

        if ! command -v service_start >/dev/null 2>&1; then
            log "service $name sans service_start, ignoré"
            continue
        fi

        service_start || { log "service $name : démarrage refusé"; return 1; }
        [ -n "$service_pid" ] && pids="$pids $service_pid"

        if command -v service_ready >/dev/null 2>&1; then
            i=0
            while [ "$i" -lt "$SERVICE_READY_TIMEOUT" ]; do
                if service_ready; then break; fi
                if [ -n "$service_pid" ] && ! kill -0 "$service_pid" 2>/dev/null; then
                    log "service $name : arrêté pendant son démarrage"
                    return 1
                fi
                i=$((i + 1))
                sleep 1
            done
            if [ "$i" -ge "$SERVICE_READY_TIMEOUT" ]; then
                log "service $name : toujours pas prêt après ${SERVICE_READY_TIMEOUT}s"
                return 1
            fi
        fi

        log "service $name prêt"
    done
}

terminate() {
    # shellcheck disable=SC2086
    [ -n "$pids" ] && kill $pids 2>/dev/null || true
    [ -n "${main_pid:-}" ] && kill "$main_pid" 2>/dev/null || true
    exit 0
}
trap terminate INT TERM

start_services

log "démarrage d'Automata"
/usr/local/bin/automata "$@" &
main_pid=$!

# Surveillance : la mort du processus principal ou de n'importe quel
# service arrête le conteneur, avec le code qui convient.
while kill -0 "$main_pid" 2>/dev/null; do
    for pid in $pids; do
        if ! kill -0 "$pid" 2>/dev/null; then
            log "un service annexe s'est arrêté, arrêt du conteneur"
            kill "$main_pid" 2>/dev/null || true
            wait "$main_pid" 2>/dev/null || true
            exit 1
        fi
    done
    sleep 5
done

wait "$main_pid"
status=$?
log "Automata s'est arrêté (code $status)"
# shellcheck disable=SC2086
[ -n "$pids" ] && kill $pids 2>/dev/null || true
exit $status
