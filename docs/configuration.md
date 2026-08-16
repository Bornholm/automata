# Référence de configuration

Un seul fichier YAML décrit toute l'instance. Ce document parcourt chaque
section dans l'ordre où elle apparaît dans `config/config.example.yaml`.

Trois règles valent partout.

Les valeurs sensibles se référencent par variable d'environnement, avec la
syntaxe `${NOM}`. L'expansion s'applique au fichier entier, commentaires
compris, et une variable absente est une erreur de démarrage.

Les chemins relatifs sont résolus depuis le répertoire du fichier de
configuration.

`automata config validate -config <fichier>` rejette la configuration avant
tout accès réseau ou disque, et affiche toutes les erreurs d'un coup.

## version

```yaml
version: 1
```

Seule la valeur `1` est acceptée. Ce champ existe pour qu'une future
incompatibilité soit détectée au démarrage plutôt qu'à l'exécution.

## organization

```yaml
organization:
  id: home
  display_name: Maison
```

`id` sert d'`org_id` par défaut et apparaît dans l'audit. `display_name` est
donné aux agents dans leur bloc de contexte, pour qu'ils sachent au nom de qui
ils parlent.

## storage

```yaml
storage:
  application:
    driver: sqlite
    path: /data/app.sqlite
    pragmas:
      foreign_keys: true
      journal_mode: WAL
      busy_timeout: 5s
```

Base applicative : conversations, messages, pièces jointes, plans d'actions,
exécutions planifiées, tentatives de livraison, audit.

`driver` accepte `sqlite` ou `sqlite3`, les deux visent le même driver Go pur.

Gardez les trois pragmas. `foreign_keys` fait respecter les liens entre
tables, `WAL` évite qu'une lecture bloque une écriture, `busy_timeout` laisse
une transaction attendre au lieu d'échouer immédiatement.

Le fichier est créé en 0600 et son répertoire en 0700.

## courier

```yaml
courier:
  providers:
    whatsapp:
      type: whatsapp
      session_path: /data/courier/whatsapp
```

Chaque entrée est un fournisseur de messagerie. Le nom de la clé,
`whatsapp` ici, est celui que vous réutilisez dans `origins`, `channels` et
`schedules[].delivery.provider`.

`session_path` conserve la liaison d'appareil. Supprimez ce répertoire et il
faudra scanner un nouveau QR code. Sauvegardez-le.

Seul le type `whatsapp` est livré.

## observability

```yaml
observability:
  enabled: true
  addr: 127.0.0.1:9090
```

Démarre un serveur HTTP local exposant trois routes :

| Route | Réponse |
|---|---|
| `GET /healthz/live` | 200 dès que le processus tourne |
| `GET /healthz/ready` | 200 quand persistance et pipelines sont démarrés, 503 sinon |
| `GET /metrics` | Compteurs agrégés en JSON |

Aucune de ces routes n'expose de contenu de message, de transcription ou
d'argument d'outil.

Laissez cette section active si vous utilisez l'image Docker : sa sonde de
santé interroge `/healthz/ready`. Écouter sur `127.0.0.1` suffit, la sonde
s'exécute dans le conteneur.

## audio

```yaml
audio:
  enabled: true
  transcription_client: transcription
  max_size: 20MiB
  timeout: 2m
  persist_audio: false
  persist_transcription: false
```

Traite les notes vocales et les fichiers audio joints. Les deux sont
transcrits : joindre un `.mp3` plutôt qu'appuyer sur le bouton micro ne change
pas l'intention.

`transcription_client` désigne une entrée de `llm_clients`.

Le flux est lu de façon bornée, transcrit, puis les octets sont abandonnés.
Rien n'est écrit sur disque. Avec `persist_transcription: false`, la base
conserve seulement `[Message vocal transcrit pour traitement]`.

Ne passez ces deux drapeaux à `true` que si vous savez pourquoi. Le texte
transcrit est souvent plus sensible que le message écrit équivalent, parce
qu'on parle plus librement qu'on n'écrit.

## attachments

```yaml
attachments:
  enabled: true
  max_size: 8MiB
  max_count: 4
  accepted_types:
    - image/png
    - image/jpeg
    - image/webp
    - image/gif
    - text/plain
  max_history: 4
  max_reply: 3
```

Pièces jointes non vocales. Désactivé par défaut : sans cette section, une
image est écartée, et l'agent en est informé pour pouvoir l'expliquer.

`accepted_types` mérite votre attention. Un fournisseur refuse la requête
**entière** quand une pièce jointe ne lui convient pas, ce qui laisse
l'utilisateur sans réponse. Le filtre existe pour écarter ces pièces avant
l'appel. Alignez-le sur ce que votre modèle accepte vraiment : le provider
OpenAI admet png, jpeg, webp, gif et les documents `text/*` seulement, un PDF
sera rejeté.

Le filtre porte sur le type MIME déclaré par la plateforme, pas sur les octets
reçus. Un fichier annoncé `image/png` qui n'en est pas un atteindra le
fournisseur, qui le rejettera.

`max_history` contrôle le rejeu. Les pièces jointes sont conservées en base et
renvoyées au modèle aux tours suivants, ce qui permet de dire « et sur la
photo d'avant ? ». Chaque image rejouée est retransmise à chaque message
suivant : la valeur pèse directement sur le coût. `0` désactive le rejeu sans
désactiver la réception.

Conserver des images a un coût en vie privée et en volume. Voir
[operations.md](operations.md) pour la surveillance et la purge, et
[security-model.md](security-model.md) pour l'analyse.

## llm_clients

```yaml
llm_clients:
  main:
    provider: openai
    model: ${MAIN_MODEL}
    api_key: ${MAIN_API_KEY}
    base_url: ${MAIN_BASE_URL}

  transcription:
    provider: openai
    model: ${TRANSCRIPTION_MODEL}
    api_key: ${TRANSCRIPTION_API_KEY}
```

`provider` accepte `openai`, `mistral` et `openrouter`. `base_url` est
facultative et permet de viser un service compatible OpenAI.

Rien n'oblige tous les agents à partager un client. Donner un modèle rapide et
bon marché à un spécialiste qui ne fait que reformuler, et un modèle plus
capable à l'orchestrateur, est un réglage courant.

## agents

Détaillé dans [agents.md](agents.md). Résumé des champs :

| Champ | Rôle |
|---|---|
| `type` | `orchestrator` (peut déléguer) ou `specialist` (peut porter des MCP) |
| `client` | Entrée de `llm_clients` |
| `system_prompt.file` ou `.inline` | Personnalité et mission. Exactement une des deux |
| `delegates` | Noms des spécialistes joignables. Orchestrateur seulement |
| `memory` | Drapeaux `search`, `remember`, `forget` |
| `mcp_servers` | Serveurs MCP autorisés. Spécialiste seulement |
| `capabilities` | Permissions applicatives de l'agent |
| `limits` | Plafonds d'exécution, tous obligatoires |

Les limites, sans exception :

```yaml
limits:
  max_sequential_tool_calls: 8
  max_actions_per_turn: 10
  tool_timeout: 30s
  max_tool_result_bytes: 16KiB
  max_tool_context_bytes: 64KiB
```

`max_sequential_tool_calls` borne les tours d'appels d'outils enchaînés.
Atteint, le tour échoue au lieu de boucler.

`max_actions_per_turn` borne le lot d'actions soumises à confirmation. Un
dépassement rejette le lot entier, jamais un préfixe : confirmer dix actions
sur douze annoncées serait pire que tout refuser.

`max_tool_result_bytes` plafonne **un** résultat d'outil.
`max_tool_context_bytes` plafonne leur **cumul** sur le tour. Les deux sont
nécessaires : huit appels tenant chacun sous le plafond unitaire dépassent
largement le contexte prévu. Toute réduction est signalée au modèle.

## mcp_servers

```yaml
mcp_servers:
  google-calendar:
    transport: http
    url: ${GOOGLE_CALENDAR_MCP_URL}
    headers:
      Authorization: Bearer ${GOOGLE_CALENDAR_MCP_TOKEN}
```

Seul le transport `http` existe. Pas de `stdio`, pas de serveur lancé en
sous-processus.

Les en-têtes déclarés ici valent pour tout le monde. Pour donner à chaque
utilisateur son propre jeton, voir la section correspondante d'
[agents.md](agents.md).

Deux noms sont reconnus par l'application et déclenchent un traitement
particulier, la résolution de ressource et le flux de confirmation :
`google-calendar` et `todo`. Les autres sont exposés tels quels au
spécialiste.

## memory

```yaml
memory:
  store:
    driver: sqlite
    path: /data/amoxtli.sqlite
  indexes:
    - id: lexical
      type: bleve
      path: /data/memory.bleve
      weight: 1
  policies:
    private_can_write_org: false
    org_readable_by_children: true
```

Mémoire persistante, portée par Amoxtli. Le store garde le contenu, l'index
sert la recherche. Les deux doivent être sauvegardés ensemble : un index sans
store ne vaut rien, un store sans index rend les souvenirs introuvables
jusqu'à `automata memory reindex`.

`private_can_write_org` à `true` autoriserait une conversation privée à écrire
dans la mémoire commune. Le plan l'interdit et la valeur par défaut le reflète.

`org_readable_by_children` laisse un rôle restreint lire la mémoire commune.

## identities

```yaml
identities:
  roles:
    adult:
      permissions:
        - memory.personal.read
        - memory.personal.write
        - calendar.group.write
  principals:
    - id: alice
      kind: human
      display_name: Alice
      roles: [adult]
```

Une permission s'écrit `<domaine>.<portée>.<action>`. La portée vaut
`personal`, `group` ou `org`. L'action vaut `read`, `write` ou `delete`. Le
domaine est libre : `memory`, `calendar`, `todo`, ou le vôtre.

`kind` vaut `human` ou `service`. Réservez `service` aux principaux utilisés
par les tâches planifiées, avec le strict minimum de permissions.

Un principal peut aussi porter ses propres connexions MCP, décrites dans
[agents.md](agents.md).

## origins

```yaml
origins:
  - provider: whatsapp
    external_user_id: ${ALICE_WHATSAPP_ID}
    principal_id: alice
```

Associe une identité de plateforme à un principal. C'est la seule table qui
décide qui est qui.

Un message dont l'origine n'est pas déclarée ici est ignoré sans le moindre
appel au modèle. C'est la première barrière, et la moins coûteuse.

## channels

```yaml
channels:
  - provider: whatsapp
    channel_id: ${ORG_GROUP_CHANNEL_ID}
    display_name: Groupe principal
    kind: group
    org_id: home
    scope: group
    scope_id: main-group
    activation: mention
    members: [alice, leo]
    resources:
      calendar: ${ORG_GROUP_CALENDAR_ID}
      todo: ${ORG_GROUP_TODO_ID}

  - provider: whatsapp
    channel_id: ${ALICE_PRIVATE_CHANNEL_ID}
    kind: private
    org_id: home
    scope: personal
    scope_id: alice
    principal_id: alice
    resources:
      calendar: ${ALICE_CALENDAR_ID}
```

Un canal absent de cette liste est refusé, même si son auteur est connu.

`scope` et `scope_id` déterminent à quelles ressources la conversation donne
accès. Un canal privé exige `principal_id`, un canal de groupe exige
`members`.

`activation: mention` impose la mention explicite de l'assistant. Sans
mention, aucun appel au modèle.

`resources` associe la portée aux identifiants externes réels. Ces
identifiants ne sont jamais montrés au modèle ni acceptés de sa part :
l'application les résout, y compris une seconde fois au moment d'exécuter une
action confirmée. Si un modèle glisse un `calendar_id` dans ses arguments, il
est écarté.

La portée `org` n'a pas de section dédiée. Déclarez un canal avec
`scope: org` et `scope_id` égal à l'identifiant d'organisation, portant les
ressources communes.

## schedules

```yaml
schedules:
  - id: morning-summary
    enabled: true
    schedule:
      cron: "0 7 * * *"
      timezone: Europe/Paris
    execution:
      principal_id: scheduler-readonly
      org_id: home
      scope: org
      scope_id: home
      agent: main
      prompt: |
        Prépare un résumé des événements de la journée.
      actions:
        policy: read_only
    delivery:
      provider: whatsapp
      channel_id: ${ORG_GROUP_CHANNEL_ID}
      mode: on_content
    concurrency:
      policy: forbid
      timeout: 10m
```

Le fuseau est obligatoire, jamais déduit du système.

`actions.policy` vaut `read_only`, les actions proposées sont journalisées
puis ignorées, ou `require_confirmation`, elles deviennent un plan qu'un
humain habilité du canal de livraison peut confirmer. Aucune écriture
autonome n'existe dans les deux cas.

`delivery.mode` vaut `always`, `on_content` (rien n'est envoyé si l'agent n'a
rien produit) ou `on_failure`.

`concurrency.policy: forbid` empêche une occurrence de démarrer si la
précédente tourne encore. `timeout` borne l'exécution.

Une occurrence est enregistrée avant d'être exécutée, et la contrainte
`UNIQUE(schedule_id, scheduled_for)` interdit de la rejouer.

### Le piège du changement d'heure

Évitez les heures entre 02:00 et 02:59 sur un fuseau qui pratique l'heure
d'été. C'est la plage vécue deux fois au passage à l'heure d'hiver.

Automata protège ce cas : une expression visant une heure unique, comme
`0 7 * * *` ou `30 2 * * *`, n'est déclenchée qu'une fois même si son heure
murale se répète. Les expressions à cadence, comme `0 * * * *`, gardent au
contraire toutes leurs occurrences réelles, parce que la journée en compte
légitimement une de plus. Une heure hors de cette plage évite d'avoir à y
penser.

## Ordre de vérification au démarrage

Pour situer une erreur, voici l'ordre réel des opérations :

1. Lecture du fichier et expansion des variables d'environnement.
2. Résolution des chemins relatifs, chargement des fichiers de prompts.
3. Validation complète, toutes erreurs agrégées.
4. Ouverture de la base et migrations.
5. Récupération des plans interrompus et des verrous périmés.
6. Construction des clients LLM, du gestionnaire MCP, des agents.
7. Démarrage des pipelines, du scheduler et du serveur d'observabilité.

Toute erreur avant l'étape 4 est une erreur de configuration : le processus
s'arrête sans avoir rien touché.
