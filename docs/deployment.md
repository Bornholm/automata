# Déploiement — Automata

Comment construire l'image Docker d'Automata et la déployer sur une machine
locale unique avec `docker compose`. Pour la sauvegarde, la
restauration et les procédures d'exploitation courantes (mise à jour,
diagnostic de panne, endpoints de santé), voir `docs/operations.md` — ce
document ne les duplique pas.

Pour un déploiement sur une instance Dokku, voir
[misc/dokku/README.md](../misc/dokku/README.md) : `make dokku-deploy` y
contourne le problème décrit juste en dessous en vendorisant les dépendances
dans le commit poussé, plutôt qu'en fournissant des contextes de build.

## 1. Dépendances et binaires publiés

Toutes les dépendances sont des modules Go publiés : le dépôt se construit
seul, sans dépôt frère ni contexte de build supplémentaire.

```bash
go build ./...
docker build .
```

Chaque version taguée publie sur GitHub des binaires prêts à l'emploi
(Linux, macOS, Windows ; amd64, arm64, arm, 386), des paquets `.deb` et
Arch, et une image de conteneur minimale `ghcr.io/bornholm/automata`
construite avec ko sur une base distroless. Les plugins de référence sont
publiés séparément — archives `automata-plugin-<nom>_…` et paquets
`automata-plugin-<nom>`, installés dans `/usr/lib/automata/plugins` — parce
qu'ils n'ont pas la même licence (Apache 2.0) et qu'on n'en veut pas
toujours tous. `automata version` affiche la version du binaire installé.

L'image `ghcr.io/bornholm/automata` ne contient que le binaire : elle
convient si vous apportez vos propres services (plugins, LeaSH, recherche
web). L'image de `misc/dokku/Dockerfile` embarque en plus les plugins et
SearXNG, pour un déploiement complet en un conteneur.

## 2. Volumes et configuration

Trois volumes, comme prescrit par plan de conception, Phase 22 :

| Volume      | Accès          | Contenu                                                        |
|-------------|----------------|------------------------------------------------------------------|
| `/data`     | lecture-écriture | `app.sqlite`, `amoxtli.sqlite`, `memory.bleve/`, `courier/` (voir `docs/operations.md` §1 pour le détail et la procédure de sauvegarde) |
| `/config`   | lecture seule  | Fichier(s) de configuration YAML                                 |
| `/prompts`  | lecture seule  | Fichiers de system prompts (`system_prompt.file`)                |

### Contrainte importante : `/prompts` est un volume séparé de `/config`

`internal/config` résout les chemins relatifs (`storage.application.path`,
`memory.store.path`, `memory.indexes[].path`, `agents.*.system_prompt.file`) **par rapport au répertoire du fichier de
configuration** (`internal/config/resolve.go`, fonctions `resolvePath` et
`loadSystemPrompts`). Dans ce déploiement, `/config` et `/prompts` sont deux
volumes distincts : un chemin `system_prompt.file: prompts/main.md` se
résoudrait donc à `/config/prompts/main.md`, qui n'existe pas dans ce
conteneur. **La configuration doit référencer les prompts par un chemin
absolu `/prompts/...`**, qui n'est jamais réécrit (`resolvePath` laisse un
chemin absolu inchangé). De même, `storage.application.path`,
`memory.store.path` et `memory.indexes[].path` doivent être des chemins
absolus sous `/data/...` pour éviter toute résolution accidentelle par rapport à
`/config`.

Exemple de configuration cohérent avec ces trois volumes :

```yaml
version: 1

organization:
  id: home
  display_name: Maison

storage:
  application:
    driver: sqlite
    path: /data/app.sqlite
    pragmas:
      foreign_keys: true
      journal_mode: WAL
      busy_timeout: 5s

agents:
  main:
    type: orchestrator
    system_prompt:
      file: /prompts/main.md          # chemin absolu : /prompts est un
                                        # volume distinct de /config
    limits:
      max_sequential_tool_calls: 8
      max_actions_per_turn: 10
      tool_timeout: 30s
      max_tool_result_bytes: 16KiB
      max_tool_context_bytes: 64KiB

memory:
  store:
    driver: sqlite
    path: /data/amoxtli.sqlite
  indexes:
    - id: lexical
      type: bleve
      path: /data/memory.bleve
      weight: 1
```

### Point de départ : `config/config.example.yaml`

Le dépôt fournit une configuration complète et commentée, couvrant toutes
les sections (identités, rôles, origines, canaux, ressources, agents,
serveurs MCP, mémoire, planification, observabilité), ainsi que les quatre
fichiers de prompts correspondants dans `prompts/`.

```bash
cp config/config.example.yaml config/config.yaml
```

`config/config.yaml` est ignoré par git (voir `.gitignore`) : il porte les
choix d'un déploiement donné. Les secrets ne figurent dans aucun des deux
fichiers — ils sont référencés par variables d'environnement et lus au
chargement.

Cet exemple référence les prompts par `../prompts/...` plutôt que par
`/prompts/...` : depuis `/config/config.yaml`, la résolution donne bien
`/prompts/...` dans le conteneur, et le même fichier reste utilisable
directement depuis le dépôt, où `config/` et `prompts/` sont côte à côte.
Un chemin absolu `/prompts/...` reste évidemment valable si la
configuration n'est destinée qu'au conteneur.

Valider avant tout démarrage — la commande échoue sur la moindre variable
absente, référence inconnue ou expression cron invalide, avant toute
connexion externe :

```bash
automata config validate -config config/config.yaml
```

**Piège à connaître** : l'expansion des variables d'environnement s'applique
au fichier entier, **y compris aux commentaires**. Une référence
d'environnement écrite dans un commentaire, même à titre d'illustration,
fait échouer le chargement si la variable n'existe pas.

## 3. Construire l'image

```bash
docker build -t automata:local .
```

Rien d'autre : ni Buildx, ni `--build-context`, ni dépôt frère. Le build a
été réexécuté après la suppression des `replace` — succès, image finale
d'environ 125 Mo.

## 4. Propriétaire du volume `/data`

L'image finale tourne en utilisateur non privilégié `nonroot` (UID/GID
65532, image distroless). Un volume Docker nommé, créé implicitement au
premier `docker run`/`docker compose up`, appartient par défaut à
`root:root` avec des permissions `0755` : le processus ne peut alors pas y
créer `app.sqlite` (`permission denied`). **Avant le tout premier
démarrage**, changer le propriétaire du volume :

```bash
docker volume create automata-data   # ou laisser compose.yaml le créer
docker run --rm --user 0:0 -v automata-data:/data busybox chown -R 65532:65532 /data
```

Un montage bind (répertoire hôte) évite ce problème si le répertoire hôte
est créé avec `chown 65532:65532` au préalable, ou si le montage utilise un
UID/GID déjà accessible en écriture.

## 5. Démarrer le service

```bash
mkdir -p config prompts        # si absents : voir §2 pour leur contenu
docker compose up -d
docker compose ps              # état du conteneur
docker compose logs -f automata
```

`compose.yaml` monte `/data` (volume nommé Docker, persistant),
`./config:/config:ro` et `./prompts:/prompts:ro` (montages bind en lecture
seule). Les secrets référencés par `${...}` dans la configuration YAML
(clés API, jetons, identifiants de canaux/ressources — voir l'exemple §2 et
`internal/config` pour le mécanisme d'expansion) sont fournis via la
section `environment:` de `compose.yaml`, à alimenter par un fichier `.env`
non versionné ou par l'environnement de l'hôte.

## 6. Healthcheck

L'image finale (`gcr.io/distroless/static-debian12:nonroot`) ne contient ni
shell ni client HTTP : la forme habituelle d'une sonde (`CMD curl -f ...`) y
est impossible. Le binaire applicatif étant lui-même présent dans l'image,
c'est lui qui fournit la sonde, via sa sous-commande dédiée :

```bash
automata healthcheck [-addr 127.0.0.1:9090] [-timeout 3s]
```

Elle interroge `GET /healthz/ready` (`internal/observability`,
documenté en détail dans `docs/operations.md` §4) et n'expose qu'un code de
sortie : `0` si le service est prêt, `1` s'il ne l'est pas, s'il est
injoignable ou si le délai est dépassé. Aucun shell n'est requis, la
directive du `Dockerfile` utilise la forme exec :

```dockerfile
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/automata", "healthcheck"]
```

**Prérequis** : `observability.enabled` doit valoir `true` dans la
configuration montée, et `observability.addr` correspondre à l'adresse sondée
(`127.0.0.1:9090` par défaut). Sans cela aucun serveur HTTP n'écoute, la
sonde échoue, et le conteneur est signalé `unhealthy` — si l'observabilité
est délibérément désactivée, neutraliser la sonde côté `compose.yaml` :

```yaml
healthcheck:
  disable: true
```

L'état est ensuite visible via `docker compose ps` ou
`docker inspect --format '{{.State.Health.Status}}' <conteneur>`. La même
sonde reste bien sûr interrogeable depuis l'hôte si le port est publié :

```bash
curl -s http://127.0.0.1:9090/healthz/ready
```

## 7. Arrêt gracieux

`cmd/automata/main.go` installe déjà un contexte annulé sur `SIGINT`/
`SIGTERM` (`signal.NotifyContext`), propagé par `internal/registry.Run` à
tous les composants (pipelines ingress, scheduler, serveur
d'observabilité). `docker stop` envoie `SIGTERM` puis, après le délai de
grâce (`--time`, 10s par défaut), `SIGKILL` si le processus n'a pas
terminé : ce comportement est donc directement compatible.

**Test réellement exécuté lors de cette phase** (conteneur démarré via
`docker run` avec une configuration de test minimale valide et le volume
`/data` correctement `chown`-é, §5) : `docker stop -t 10` a bien produit,
dans l'ordre, dans les logs JSON du conteneur, `"automata stopping"` puis
`"mcp: gestionnaire fermé"` (fermeture du gestionnaire MCP,
`internal/mcp.Manager.Close`, l'un des `defer` de `internal/registry.Run`),
et le conteneur est passé à l'état `exited` avec le code de sortie `0` —
bien avant le délai de grâce de 10 s, donc sans `SIGKILL`. L'arrêt propre
déjà en place fonctionne donc identiquement à travers
Docker.

## 8. Limitation mono-instance

**Ne jamais faire tourner plusieurs instances (`replicas`/scale > 1) de ce
service sur le même volume `/data`.** La base applicative SQLite est
mono-écrivain, et le scheduler repose sur des verrous de
concurrence en mémoire, par processus — aucun verrouillage distribué n'est
implémenté (voir le commentaire dédié dans `compose.yaml`). Plusieurs
instances concurrentes corromperaient la base ou dupliqueraient les
exécutions planifiées (rappels, livraisons). Tant qu'un mécanisme de
verrouillage distribué n'est pas ajouté (hors périmètre de ce plan), ce
déploiement reste strictement mono-instance.

## 9. Sauvegarde, restauration, mise à jour

Voir `docs/operations.md` §1 à §5 : procédures de sauvegarde/restauration
des quatre emplacements sous `/data`, et procédure de mise à jour du
binaire/redémarrage. Ces procédures ne changent pas sous Docker : arrêter
proprement le conteneur (`docker compose stop` ou `docker stop`, §8
ci-dessus), copier le contenu du volume `/data`, puis le redémarrer
(`docker compose up -d` après avoir remplacé l'image par la nouvelle
version construite avec la commande du §3).
