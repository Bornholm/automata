# Déploiement — Automata

Ce document est le livrable de la Phase 22 (« Packaging et déploiement »,
PLAN.md) : comment construire l'image Docker d'Automata et la déployer sur
une machine locale unique avec `docker compose`. Pour la sauvegarde, la
restauration et les procédures d'exploitation courantes (mise à jour,
diagnostic de panne, endpoints de santé), voir `docs/operations.md` — ce
document ne les duplique pas.

## 1. Problème bloquant : dépendances locales non publiées

`go.mod` redirige trois dépendances vers des dépôts locaux via des
directives `replace` :

```
replace github.com/bornholm/go-courier => ../go-courier
replace github.com/bornholm/genai       => ../genai
replace github.com/bornholm/amoxtli     => ../amoxtli
```

Aucune des trois bibliothèques (`go-courier`, `genai`, `amoxtli`) n'a de
version taguée publiée sur un module proxy correspondant à l'API réellement
utilisée par Automata — constaté dès la Phase 5 et documenté dans
`docs/integration-inventory.md`. Conséquence directe pour le packaging :
**un `docker build` classique, avec pour seul contexte ce dépôt, échoue** à
`go mod download`/`go build`, puisque les trois modules n'existent nulle
part ailleurs que sur le disque de développement.

Vérification : dans un répertoire ne contenant que ce dépôt (sans les trois
dépôts frères sur le disque), `go build ./...` échoue avec
`go: github.com/bornholm/go-courier@...: reading ../go-courier/go.mod: open ../go-courier/go.mod: no such file or directory`
(ou l'équivalent pour `genai`/`amoxtli`) — les `replace` étant des chemins
relatifs, ils ne se résolvent que si les trois dépôts sont bien présents en
frères du répertoire de travail.

### Solution retenue (temporaire)

Les trois `replace` de `go.mod` pointent vers des **chemins relatifs**
(`../go-courier`, `../genai`, `../amoxtli`), pas vers des chemins absolus
propres à une machine (`/home/wpetit/workspace/...`). Cela fonctionne :

- **en local** : tant que les trois dépôts sont clonés en frères
  d'`automata` (`~/workspace/automata`, `~/workspace/go-courier`,
  `~/workspace/genai`, `~/workspace/amoxtli` dans cet environnement) ;
- **dans l'image Docker** : en injectant les trois dépôts comme **contextes
  de build supplémentaires** (Docker Buildx, `--build-context`) et en les
  copiant, dans l'étape de build du `Dockerfile`, à des chemins qui
  reproduisent la même disposition en frères (`/src/go-courier`,
  `/src/genai`, `/src/amoxtli`, à côté de `/src/automata`).

Alternative écartée : copier les trois dépôts *dans* le contexte de build
d'`automata` (ex. `docker/vendor/go-courier/`) et adapter les `replace` en
conséquence. Rejetée parce qu'elle aurait obligé à dupliquer physiquement
les sources sur le disque avant chaque build (ou à les committer dans
`automata`, ce qu'aucune des deux bibliothèques n'autorise ici), alors que
`--build-context` les référence directement à leur emplacement réel sans
copie manuelle ni duplication de code source. Les chemins relatifs
`--build-context` + `replace` relatifs sont la combinaison la plus simple :
un seul mécanisme (position relative des dépôts) sert à la fois le
`go build` local et le `docker build`.

**Ceci est une solution temporaire.** Une fois `go-courier`, `genai` et
`amoxtli` publiés avec une version taguée accessible sur un module proxy
standard (proxy.golang.org ou équivalent interne) :

1. retirer les trois directives `replace` de `go.mod` et remplacer les
   `require` correspondants par les versions taguées ;
2. supprimer, dans le `Dockerfile`, les trois lignes `COPY --from=gocourier
   .`/`--from=genai .`/`--from=amoxtli .` ainsi que ce paragraphe et la
   nécessité des contextes de build additionnels ;
3. la commande de build redevient un simple `docker build .`, sans
   `--build-context`.

## 2. Volumes et configuration

Trois volumes, comme prescrit par PLAN.md Phase 22 :

| Volume      | Accès          | Contenu                                                        |
|-------------|----------------|------------------------------------------------------------------|
| `/data`     | lecture-écriture | `app.sqlite`, `amoxtli.sqlite`, `memory.bleve/`, `courier/` (voir `docs/operations.md` §1 pour le détail et la procédure de sauvegarde) |
| `/config`   | lecture seule  | Fichier(s) de configuration YAML                                 |
| `/prompts`  | lecture seule  | Fichiers de system prompts (`system_prompt.file`)                |

### Contrainte importante : `/prompts` est un volume séparé de `/config`

`internal/config` résout les chemins relatifs (`storage.application.path`,
`memory.store.path`, `memory.indexes[].path`, `courier.providers.*.session_path`,
`agents.*.system_prompt.file`) **par rapport au répertoire du fichier de
configuration** (`internal/config/resolve.go`, fonctions `resolvePath` et
`loadSystemPrompts`). Dans ce déploiement, `/config` et `/prompts` sont deux
volumes distincts : un chemin `system_prompt.file: prompts/main.md` se
résoudrait donc à `/config/prompts/main.md`, qui n'existe pas dans ce
conteneur. **La configuration doit référencer les prompts par un chemin
absolu `/prompts/...`**, qui n'est jamais réécrit (`resolvePath` laisse un
chemin absolu inchangé). De même, `storage.application.path`,
`memory.store.path`, `memory.indexes[].path` et
`courier.providers.*.session_path` doivent être des chemins absolus sous
`/data/...` pour éviter toute résolution accidentelle par rapport à
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

courier:
  providers:
    whatsapp:
      type: whatsapp
      session_path: /data/courier/whatsapp

llm_clients:
  main:
    provider: openai
    model: ${MAIN_MODEL}
    api_key: ${MAIN_API_KEY}

agents:
  main:
    type: orchestrator
    client: main
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

(Voir `internal/config/testdata/valid/config.yaml` pour un exemple complet
et couvrant toutes les sections — identités, origines, canaux, agendas —
non répété ici.)

## 3. Construire l'image

Le démon Docker et `docker buildx` (≥ 0.10, fonctionnalité
`--build-context`) doivent être disponibles :

```bash
docker buildx version   # confirme la disponibilité de --build-context
```

Commande de build exacte, à exécuter depuis la racine de ce dépôt, avec les
trois dépôts frères clonés localement (adapter les chemins si les dépôts ne
sont pas sous `~/workspace`) :

```bash
docker buildx build \
  --build-context gocourier=$HOME/workspace/go-courier \
  --build-context genai=$HOME/workspace/genai \
  --build-context amoxtli=$HOME/workspace/amoxtli \
  -t automata:local \
  --load \
  .
```

`--load` importe l'image construite dans le démon Docker local (nécessaire
pour l'utiliser ensuite avec `docker compose`/`docker run`) ; à omettre si
la sortie est poussée directement vers un registre (`--push`).

Build réellement exécuté et vérifié lors de cette phase, avec les trois
dépôts frères présents sous `~/workspace` : succès (`docker images` liste
bien `automata:local`, image finale ~125 Mo).

## 4. Deux défauts corrigés par cette phase, détectés par le premier
   déploiement réel

Le packaging Docker est le premier moment où le binaire compilé est
réellement *exécuté* (par opposition à `go test`, qui construit ses propres
`config.StorageApplication`/etc. directement en Go sans jamais charger un
fichier YAML d'exemple ni lancer `registry.Run`). Deux défauts latents,
présents depuis des phases antérieures et jamais exercés par la suite de
tests existante, ont ainsi été révélés et corrigés dans le même commit que
ce packaging :

1. **`cmd/automata/main.go` : la commande racine ne démarrait jamais.**
   `automata -config /config/config.yaml` (l'invocation utilisée par
   `ENTRYPOINT`/`CMD`) échouait systématiquement avec `Execute()` retournant
   une erreur "unknown command" **avant même d'atteindre `RunE`**, avalée
   silencieusement par `SilenceErrors: true` — aucun log, code de sortie 1.
   Cause : le drapeau `-config` est en simple tiret et multi-caractères,
   donc invisible pour la résolution de commande de cobra
   (`stripFlags`/`Find`) puisqu'aucun drapeau n'est enregistré sur la
   commande racine (`DisableFlagParsing` délègue tout au paquet `flag`
   standard, dans `RunE`) ; cobra traite alors la *valeur* du drapeau
   (`/config/config.yaml`) comme une tentative de sous-commande inconnue et
   la validation par défaut (`legacyArgs`) rejette l'exécution. Corrigé en
   ajoutant `Args: cobra.ArbitraryArgs` à la commande racine (voir le
   commentaire dans `cmd/automata/main.go`), qui désactive cette validation
   pour laisser le paquet `flag` standard gérer seul les arguments, comme
   c'était déjà l'intention documentée.
2. **`internal/persistence/db.go` : `storage.application.driver: sqlite`
   (la valeur utilisée par PLAN.md §12 et tous les exemples de ce dépôt,
   `internal/config/testdata/valid/config.yaml` inclus) ne correspondait à
   aucun driver `database/sql` enregistré.** `github.com/ncruces/go-sqlite3/driver`
   ne s'enregistre lui-même que sous le nom `"sqlite3"` ; une configuration
   suivant la convention documentée du dépôt échouait donc au démarrage
   réel avec `sql: unknown driver "sqlite"`. Corrigé en enregistrant un
   alias `"sqlite"` pointant vers le même driver, dans `internal/persistence/db.go`
   (`sql.Register("sqlite", &sqlitedriver.SQLite{})`), plutôt qu'en modifiant
   tous les exemples de configuration existants pour utiliser `"sqlite3"`.

Avec ces deux corrections, le conteneur démarre réellement, journalise
`"automata starting"` en JSON structuré, et répond à `SIGTERM` (voir §8).

## 5. Propriétaire du volume `/data`

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

**Note connexe, constatée lors du test §8** : le répertoire parent de
`courier.providers.<nom>.session_path` (ex. `/data/courier/`) n'est pas créé
automatiquement par le fournisseur WhatsApp (contrairement à
`storage.application.path`, dont le répertoire parent est créé par
`persistence.Open`, `internal/persistence/db.go`). Le pipeline ingress
correspondant échoue alors au démarrage (`lstat /data/courier: no such file
or directory`) sans empêcher le reste du service de fonctionner (readiness,
scheduler, autres fournisseurs) — mais la messagerie WhatsApp restera
indisponible tant que ce répertoire n'existe pas. Créer `/data/courier/`
manuellement (ou via le même conteneur `chown`/`mkdir` du volume ci-dessus)
avant le premier démarrage évite ce problème.

## 6. Démarrer le service

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

## 7. Healthcheck

L'image finale (`gcr.io/distroless/static-debian12:nonroot`) ne contient ni
shell ni client HTTP : la forme habituelle d'une sonde (`CMD curl -f ...`) y
est impossible. Le binaire applicatif étant lui-même présent dans l'image,
c'est lui qui fournit la sonde, via sa sous-commande dédiée :

```bash
automata healthcheck [-addr 127.0.0.1:9090] [-timeout 3s]
```

Elle interroge `GET /healthz/ready` (Phase 20, `internal/observability`,
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

## 8. Arrêt gracieux

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
déjà en place depuis la Phase 1 fonctionne donc identiquement à travers
Docker.

## 9. Limitation mono-instance

**Ne jamais faire tourner plusieurs instances (`replicas`/scale > 1) de ce
service sur le même volume `/data`.** La base applicative SQLite est
mono-écrivain, et le scheduler (Phase 16/18) repose sur des verrous de
concurrence en mémoire, par processus — aucun verrouillage distribué n'est
implémenté (voir le commentaire dédié dans `compose.yaml`). Plusieurs
instances concurrentes corromperaient la base ou dupliqueraient les
exécutions planifiées (rappels, livraisons). Tant qu'un mécanisme de
verrouillage distribué n'est pas ajouté (hors périmètre de ce plan), ce
déploiement reste strictement mono-instance.

## 10. Sauvegarde, restauration, mise à jour

Voir `docs/operations.md` §1 à §5 : procédures de sauvegarde/restauration
des quatre emplacements sous `/data`, et procédure de mise à jour du
binaire/redémarrage. Ces procédures ne changent pas sous Docker : arrêter
proprement le conteneur (`docker compose stop` ou `docker stop`, §8
ci-dessus), copier le contenu du volume `/data`, puis le redémarrer
(`docker compose up -d` après avoir remplacé l'image par la nouvelle
version construite avec la commande du §3).
