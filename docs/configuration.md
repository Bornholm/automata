# Référence de configuration

## La frontière : le fichier décrit la machine, l'administration le reste

Le fichier YAML ne configure que ce qui démarre le processus : stockage et
chiffrement, ports et adresses, limites techniques, sauvegardes,
observabilité — plus, pour cette phase, la définition des agents (prompts,
délégations, serveurs MCP) et les rôles/permissions.

Tout ce qui décrit un client ou un réglage d'exploitation vit en base et
s'administre en ligne : les organisations et leurs membres, les comptes de
messagerie, les modèles et leur affectation à chaque agent
([models.md](models.md)), les tarifs, les compétences, l'activation des
plugins. Il n'y a AUCUN semis : rien ne se copie du fichier vers la base, et
ce qui est supprimé en ligne ne réapparaît jamais au redémarrage. Deux
exceptions assumées : les compétences embarquées (contenu du projet, semées
par go:embed) et le bootstrap MANUEL `automata web bootstrap`, qui importe
une première organisation déclarée dans le fichier — un geste explicite,
pas un mécanisme silencieux.

Ce document parcourt chaque section du fichier dans l'ordre où elle
apparaît dans `config/config.example.yaml`.

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

## organizations

Une instance peut servir plusieurs organisations — une maison et une équipe
de travail, par exemple — sur les mêmes agents et le même processus.

```yaml
organizations:
  - id: home
    display_name: Maison
  - id: work
    display_name: Bureau
```

Cette section peut être **entièrement absente** quand l'interface web est
active (`web.enabled`) : les organisations sont alors créées depuis
l'administration, les membres s'y rattachent par jeton et les canaux se
lient dynamiquement — c'est le mode d'un déploiement multi-organisations,
où une organisation déclarée en dur n'aurait servi qu'à passer le
chargement avant d'encombrer la liste réelle.

Dès que le fichier désigne une organisation, en revanche, elle doit
exister : `channels`, `identities.principals`, `schedules` et les
surcharges `system_prompt.org_overrides` portent toutes un `org_id`. Un
identifiant sans organisation correspondante ne se manifesterait
qu'au premier message reçu, sous la forme d'un refus d'autorisation
difficile à relier à sa cause — l'erreur de chargement nomme la section
fautive.

La forme abrégée reste acceptée quand il n'y en a qu'une :

```yaml
organization:
  id: home
  display_name: Maison
```

`id` est repris par `channels[].org_id` et apparaît dans l'audit.
`display_name` est donné aux agents dans leur bloc de contexte, résolu à
chaque requête depuis l'organisation du canal d'où vient le message.

**Rien ne traverse la frontière d'une organisation** : mémoire, rappels et
tâches d'une organisation sont inaccessibles depuis une autre, y compris pour
un principal membre des deux (`internal/authorization`). C'est la séparation
sur laquelle repose la cohabitation du personnel et du professionnel dans une
même instance.

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

### encryption_key

```yaml
storage:
  encryption_key: ${STORAGE_ENCRYPTION_KEY}   # au moins 32 caractères
```

Chiffre au repos les contenus personnels : messages, résumés de
conversation, rappels, pièces jointes et leurs légendes. Sans cette clé,
ils sont écrits en clair — c'est le comportement historique.

Le chiffrement est **applicatif** : les valeurs partent chiffrées vers la
base (AES-256-GCM, clé dérivée par HKDF-SHA256 avec un contexte propre aux
contenus) et en reviennent telles quelles. Un chiffrement de fichier
SQLite serait plus simple à activer, mais il adhérerait au moteur ; celui-ci
suivra tel quel vers PostgreSQL.

**Ce que cela protège :** une base volée, une sauvegarde égarée, un disque
revendu, un `sqlite3` ouvert par curiosité. **Ce que cela ne protège pas :**
un processus compromis ou un accès root pendant que le service tourne — la
clé y est. C'est un chiffrement au repos, pas un coffre-fort.

**Perdre la clé rend les contenus déjà chiffrés définitivement illisibles.**
Elle se sauvegarde à part, jamais dans le même coffre que les données, et
n'est pas dérivée de `web.session_secret` : faire tourner un secret de
session ne doit pas rendre les archives muettes.

Activer le réglage ne protège que ce qui est écrit ensuite. Pour l'historique
déjà en base :

```bash
automata storage encrypt -config config.yaml
```

L'opération est reprenable — une valeur déjà chiffrée est laissée telle
quelle — et la lecture reste transparente entre-temps : une base à moitié
migrée fonctionne, les deux formes cohabitant sans que rien ne s'en aperçoive.

La même clé protège les souvenirs de la mémoire : la base `amoxtli`
chiffre le contenu de ses documents et de ses images (chiffrement porté
par la bibliothèque, `amoxtli/crypto`, mêmes primitives — utilisable par
tout projet adossé à amoxtli, SQLite ou PostgreSQL). La commande `storage encrypt` migre
les deux bases, puis les reconstruit (`VACUUM`) : chiffrer les lignes ne
suffit pas, les pages mortes et le journal WAL gardent sinon les anciennes
versions en clair.

**Ce qui reste en clair, à dessein.** L'enveloppe : identifiants,
horodatages, noms affichés des membres (l'admin les liste et les cherche),
métadonnées de portée des souvenirs (la recherche filtre dessus). Et
l'index plein texte de la mémoire (`memory.bleve`) : il stocke les termes
du texte pour pouvoir chercher — le chiffrer casserait la recherche. Le
répertoire de données doit donc rester protégé par les permissions du
système ; le chiffrement de la base protège ses copies, pas la machine.

## courier

```yaml
courier:
  coalesce_window: 2s
```

Il ne reste ici que la fenêtre de coalescence : les messages texte
consécutifs d'un même expéditeur arrivés pendant cette fenêtre sont
fusionnés en un seul tour de conversation. `0s` désactive, deux secondes
par défaut.

Les **comptes de messagerie** (WhatsApp, Signal, Rocket.Chat, Discord,
mail) ne se déclarent plus dans ce fichier : ils se créent dans
l'administration (« Canaux et plateformes »), qui range leur configuration
chiffrée en base et applique les changements à chaud. Un compte supprimé
en ligne ne réapparaît jamais au redémarrage. Les noms de comptes restent
ce que `origins`, `channels` et `schedules[].delivery.provider`
référencent, et `web.mail_provider` désigne un compte de type `mail` de
cette même table.

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
  max_size: 20MiB
  timeout: 2m
  persist_audio: false
  persist_transcription: false
```

Traite les notes vocales et les fichiers audio joints. Les deux sont
transcrits : joindre un `.mp3` plutôt qu'appuyer sur le bouton micro ne change
pas l'intention.

Le modèle de transcription est le rôle d'instance `transcription`, réglé à
l'écran « Modèles » (voir [models.md](models.md)) ; il n'y a rien à
désigner ici.

Le flux est lu de façon bornée, transcrit, puis les octets sont abandonnés.
Rien n'est écrit sur disque. Avec `persist_transcription: false`, la base
conserve seulement `[Message vocal transcrit pour traitement]`.

Ne passez ces deux drapeaux à `true` que si vous savez pourquoi. Le texte
transcrit est souvent plus sensible que le message écrit équivalent, parce
qu'on parle plus librement qu'on n'écrit.

Trois échecs viennent de l'audio lui-même et reçoivent une réponse qui dit
quoi faire, plutôt que le repli générique « réessaie dans quelques
instants » — qui serait un mauvais conseil, réessayer à l'identique donnant
le même résultat :

| Cause | Réponse envoyée |
|---|---|
| Transcription vide (inaudible, silence) | proposition de réenregistrer plus près du micro, ou d'écrire |
| Dépassement de `max_size` | proposition de refaire plus court, ou d'écrire |
| Format non reconnu | proposition de réenregistrer depuis l'application, ou d'écrire |

Ces trois cas sont journalisés en `WARN`, pas en `ERROR` : un micro mal placé
n'est pas une panne, et ne doit pas déclencher d'alerte. Tout le reste —
fournisseur injoignable, dépassement de `timeout`, configuration fautive —
reste une erreur, avec le message de repli générique.

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
  tool_types:
    - video/mp4
    - video/webm
    - video/quicktime
  max_tool_size: 16MiB
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

`tool_types` est l'autre moitié du problème. Une vidéo reçue par messagerie
n'a rien à faire dans une requête au modèle — le fournisseur la refuserait,
ou la facturerait pour rien — mais on veut quand même pouvoir la traiter. Les
types listés là sont retenus à la réception, conservés comme les autres
pièces jointes, et **jamais** convertis vers le modèle : celui-ci n'en reçoit
que la liste (nom, type, taille), et un sous-agent de plugin va chercher les
octets lui-même par les outils `import_attachment` / `attach_file` (voir la
section `plugins`). Un même type ne peut pas figurer à la fois dans
`accepted_types` et dans `tool_types` : la configuration est refusée.

`max_tool_size` borne ces pièces séparément de `max_size`. Elles ne coûtent
aucun jeton, elles peuvent donc être bien plus grosses qu'une image envoyée
au modèle : `16MiB` correspond à la limite d'une vidéo WhatsApp. C'est aussi
la borne appliquée aux fichiers qu'un plugin renvoie en pièce jointe.

`max_history` contrôle le rejeu. Les pièces jointes sont conservées en base et
renvoyées au modèle aux tours suivants, ce qui permet de dire « et sur la
photo d'avant ? ». Chaque image rejouée est retransmise à chaque message
suivant : la valeur pèse directement sur le coût. `0` désactive le rejeu sans
désactiver la réception.

Conserver des images a un coût en vie privée et en volume. Voir
[operations.md](operations.md) pour la surveillance et la purge, et
[security-model.md](security-model.md) pour l'analyse.

## llm_clients, image_clients (supprimées)

Ces sections n'existent plus : le catalogue de modèles vit en base et
s'administre en ligne, clés d'API comprises (scellées au repos). Les champs
`agents.<nom>.client`, `plugins.client`, `plugins.vision_client`,
`audio.transcription_client`, `conversation.compaction.client`,
`memory.consolidation.client`, `memory.retrieval.client` et
`memory.indexes[].client` ont disparu avec elles : chaque usage est un
**rôle de l'instance**, réglé à l'écran « Modèles ». Voir
[models.md](models.md).

## agents

Détaillé dans [agents.md](agents.md). Résumé des champs :

| Champ | Rôle |
|---|---|
| `type` | `orchestrator` (peut déléguer) ou `specialist` (peut porter des MCP) |
| `description` | Ce que sait faire ce spécialiste, en une phrase. Voir ci-dessous |
| `system_prompt.file` ou `.inline` | Personnalité et mission. Exactement une des deux |
| `delegates` | Noms des spécialistes joignables. Orchestrateur seulement |
| `memory` | Drapeaux `search`, `remember`, `forget`, `history`, `recall` |
| `reminders` | Expose les outils de rappels ponctuels. Orchestrateur seulement |
| `scheduled_tasks` | Expose les outils de tâches planifiées (l'agent travaille à l'échéance). Orchestrateur seulement |
| `mcp_servers` | Serveurs MCP autorisés. Spécialiste seulement |
| `image_generation.client` | Donne l'outil `generate_image` (entrée d'`image_clients`). Spécialiste seulement |
| `requires_attachments` | Ce spécialiste est inutile sans pièce jointe : l'orchestrateur ne le sollicite pas quand le tour n'en porte aucune. Spécialiste seulement. Voir ci-dessous |
| `capabilities` | Permissions applicatives de l'agent |
| `limits` | Plafonds d'exécution, tous obligatoires |

### description

`description` est reprise dans la description de l'outil `delegate_to_<nom>`
exposé à l'orchestrateur — c'est ce que le modèle lit pour décider de
déléguer :

```yaml
  research:
    type: specialist
    description: cherche des informations à jour sur Internet et lit des pages web
```

Sans elle, le modèle ne connaît du délégué que son nom, et un petit modèle
préfère alors répondre qu'il ne sait pas faire (« je n'ai pas accès à
Internet en temps réel ») plutôt qu'appeler un outil dont il ignore la
portée — le spécialiste est là, opérationnel, et n'est jamais sollicité.
Formulez-la à la troisième personne, en complétant « le spécialiste `x`, qui
… ». Sans effet sur un orchestrateur, qui n'est le délégué de personne.

### requires_attachments

Un spécialiste qui ne sait travailler que sur une image ou un document — le
lecteur d'images, un transcripteur — n'a rien à répondre quand le tour n'en
porte aucun. Le déclarer ici fait refuser la délégation **avant** de
l'exécuter : l'orchestrateur reçoit un résultat d'outil qui le lui dit, et
aucun appel au modèle n'est facturé.

```yaml
agents:
  vision:
    type: specialist
    client: vision
    requires_attachments: true
```

Ce refus n'est pas une précaution de confort. Sollicité sans image, un
modèle multimodal ne constate pas qu'il ne voit rien : il complète la
description la plus plausible. En production le 2026-08-23, un « Mon
profil » ambigu a déclenché une délégation à vide, et la vision a décrit un
petit-déjeuner entièrement inventé, que l'orchestrateur a relayé de bonne
foi. Le prompt du spécialiste porte la même consigne en ceinture, mais
elle demande au modèle qui invente de constater lui-même son invention :
c'est au code de ne pas poser une question dont il sait qu'elle est sans
matière.

L'orchestrateur ne connaît aucun spécialiste par son nom — il interroge une
capacité déclarée, comme pour les fichiers de l'historique.

### image_generation

```yaml
  imagine:
    type: specialist
    description: generates images from text descriptions
    client: main
    system_prompt:
      file: ../prompts/imagine.md
    image_generation:
      client: image
    limits:
      tool_timeout: 60s   # générer une image dépasse largement une complétion
      # ...
```

Le spécialiste reçoit l'outil `generate_image` (prompt + ratio d'aspect).
L'image produite est jointe au résultat d'outil et remonte automatiquement
jusqu'au canal, à travers la délégation : l'utilisateur reçoit l'image
elle-même, pas une description. Un échec de génération est expliqué au
modèle plutôt que de faire échouer le tour.

### reminders

`reminders: true` donne à l'agent trois outils : `create_reminder` (« 
rappelle-moi demain à 9h de sortir les poubelles »), `list_reminders` et
`cancel_reminder`. À l'échéance, le message du rappel est envoyé sur le
canal où il a été demandé — jamais ailleurs, la destination n'est pas un
choix du modèle. Il vit dans la base applicative (table `reminders`),
survit aux redémarrages, et un rappel devenu échu pendant un arrêt part dès
le démarrage suivant.

Un rappel peut être **récurrent** (« chaque mardi soir ») : il porte alors
une expression cron standard et un fuseau IANA, le même dialecte que
`schedules`. L'application calcule elle-même la première occurrence, puis
réarme l'échéance après chaque envoi — le rappel reste actif jusqu'à son
annulation par `cancel_reminder`. Le fuseau garantit que « chaque mardi
20h » reste 20h à travers les changements d'heure. Après un long arrêt du
worker, une seule livraison de rattrapage part, jamais une rafale : la
prochaine occurrence est calculée depuis l'instant courant. La différence
avec `schedules` : un schedule exécute un agent à heure fixe, un rappel
récurrent renvoie un texte figé — et se crée en conversation, sans toucher
à la configuration.

Chaque appel d'outil est autorisé par principal via les permissions du
domaine `reminder` (`reminder.personal.write`, `reminder.group.read`, etc.,
voir `identities.roles`) : activer le drapeau sans accorder ces permissions
donne des outils qui refusent poliment. Un rappel n'est visible et annulable
que depuis sa conversation d'origine.

### scheduled_tasks

`scheduled_tasks: true` ajoute `schedule_task`, `list_scheduled_tasks` et
`cancel_scheduled_task`. Une tâche planifiée partage tout avec un rappel —
échéance, récurrence cron, fuseau, annulation, cloisonnement par
conversation — sauf l'essentiel : à l'échéance, l'agent **travaille**, avec
ses outils et ses délégués, et c'est sa réponse qui est envoyée.

C'est la différence qui compte à l'usage. « Chaque matin, un bulletin
météo » n'est pas un rappel : un rappel enverrait tous les jours le texte
« bulletin météo ». Une tâche va chercher la météo du jour.

```yaml
  main:
    reminders: true
    scheduled_tasks: true
```

Trois garde-fous, aucun laissé au modèle :

- **Identité figée.** La tâche s'exécute sous le principal qui l'a créée,
  dans sa conversation d'origine, avec la portée du canal telle qu'elle est
  déclarée aujourd'hui — pas celle du jour de la création. Un canal qui
  change de portée emmène ses tâches avec lui. Elle ne peut donc rien faire
  que son auteur ne puisse demander en direct : c'est pourquoi son
  déclencheur (`scheduled_task`) suit les règles de son canal, là où un
  `schedules` de configuration (`cron`, principal de service que personne n'a
  nommément mandaté) reste tenu à l'écart des données personnelles.
- **Aucun outil de programmation pendant l'exécution.** À l'échéance, l'agent
  n'a ni `schedule_task` ni `create_reminder`. Sans cela, devant une consigne
  rédigée comme une demande (« Prépare un bulletin météo… »), il la
  reprogramme au lieu de l'exécuter — et une tâche peut se réarmer
  indéfiniment.
- **Lecture seule stricte.** Les actions sensibles proposées pendant un tour
  planifié sont ignorées et signalées dans le message livré. Personne n'est
  devant l'écran pour confirmer : rien ne doit pouvoir écrire dehors.
- **Permissions distinctes.** Le domaine est `task`
  (`task.personal.write`…), séparé de `reminder` : programmer un travail de
  l'assistant est un pouvoir plus large que poser un pense-bête, et ne
  s'accorde pas par mégarde avec lui.

Une tâche dont l'exécution échoue n'envoie rien et ne réarme pas sa
récurrence : elle passe en `failed`. Un bulletin quotidien qui casse
s'arrête donc visiblement, au lieu d'échouer en silence tous les matins.

Différence avec `schedules` : ceux-ci sont déclarés en configuration,
peuvent viser n'importe quel canal, n'importe quel agent et une politique
`require_confirmation` ; une tâche naît en conversation, reste dans la
sienne, et ne sait qu'observer. Les deux coexistent.

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

### system_prompt.org_overrides

```yaml
agents:
  main:
    system_prompt:
      file: ../prompts/main.md
      org_overrides:
        work:
          file: ../prompts/work-main.md
```

Un agent partagé entre plusieurs organisations peut porter une personnalité
par organisation : la variante est choisie à chaque requête selon
l'organisation du canal. Seule la personnalité change — les règles
invariantes, la section des capacités et les règles d'honnêteté sont
recomposées à l'identique dans chaque variante, aucune organisation ne peut
en être exemptée. Une organisation sans surcharge utilise le prompt par
défaut de l'agent. Les surcharges imbriquées sont refusées au chargement,
ainsi qu'une clé ne désignant aucune organisation déclarée.

## mcp_servers

```yaml
mcp_servers:
  google-calendar:
    transport: http
    url: ${GOOGLE_CALENDAR_MCP_URL}
    headers:
      Authorization: Bearer ${GOOGLE_CALENDAR_MCP_TOKEN}

    resource:
      key: calendar
      parameter: calendar_id
    permission_domain: calendar
    tools:
      confirm_writes: true
      read_prefixes: [list_, get_, search_, find_]
      require_rfc3339: [start, end]
      dedupe_writes: false
```

Trois transports existent. Deux parlent HTTP et se configurent exactement de
la même façon — `url`, `headers` — mais pas le même protocole :

| Transport | Révision du protocole MCP | Quand l'utiliser |
| --- | --- | --- |
| `http` | 2024-11-05 (HTTP+SSE) | serveurs anciens |
| `streamable-http` | 2025-03-26 et suivantes | serveurs récents, dont [misc/web-search](../misc/web-search/README.md) |

Le choix n'est pas cosmétique et aucune négociation ne le rattrape : un
serveur streamable rejette le GET permanent qu'ouvre le client SSE, et la
connexion échoue au démarrage avec une erreur du serveur. En cas de doute,
`streamable-http` est le bon premier essai pour un serveur publié depuis 2025.

Le troisième, `stdio`, fait lancer le serveur par Automata en sous-processus
local :

```yaml
mcp_servers:
  imap:
    transport: stdio
    command: ["imap-mcp", "--host", "{{host}}", "--port", "{{port}}"]
    env:
      IMAP_USER: "{{user}}"
      IMAP_PASSWORD: "{{password}}"
    permission_domain: mail
    tools:
      confirm_writes: true
      read_prefixes: [list_, get_, search_, fetch_]
```

`command` est l'exécutable puis ses arguments, sans interprétation par un
shell. `env` complète l'environnement du worker pour le processus enfant —
les secrets passent TOUJOURS par `env`, jamais par `command` : les arguments
d'un processus sont lisibles par tout processus local (`/proc/<pid>/cmdline`),
son environnement non.

Les patrons `{{nom}}` sont résolus **par principal** via
`identities.principals[].mcp.<serveur>.values` (voir [agents.md](agents.md)),
et fonctionnent sur tous les transports : `command` et `env` en stdio, `url`
et valeurs de `headers` sur les deux transports HTTP —

```yaml
meteo:
  transport: http
  url: https://mcp.example.com/tenants/{{tenant}}/mcp?api_key={{api_key}}
  headers:
    Authorization: Bearer {{token}}
```

— chacun obtient ainsi sa propre connexion, établie avec SES identifiants
(processus dédié en stdio, connexion HTTP dédiée en http). Un principal sans
`values` pour un serveur à patrons n'y a simplement pas accès — jamais de
repli sur les valeurs d'un autre, jamais de patron littéral envoyé. Un
serveur sans patron reste partagé par session, comme avant. La validation au
chargement vérifie que chaque surcharge couvre tous les patrons du serveur
et qu'aucune valeur ne reste sans patron correspondant, en ne citant que les
noms. Préférez un en-tête à une clé en variable d'URL quand le serveur le
permet : une URL fuit plus facilement (journaux de proxys et de serveurs
intermédiaires) qu'un en-tête.

Aucun nom de serveur n'a de signification pour l'application. Tout ce qui
distingue un agenda d'une recherche web se déclare dans les champs ci-dessous,
ce qui permet de brancher n'importe quel service sans écrire de code.

| Champ | Effet |
|---|---|
| `resource.key` | Clé lue dans `channels[].resources` pour la portée courante |
| `resource.parameter` | Nom du paramètre sous lequel le serveur attend cet identifiant |
| `permission_domain` | Premier segment des permissions exigées (`calendar` donne `calendar.<portée>.write`) |
| `tools.confirm_writes` | Transforme les écritures en actions à confirmer au lieu de les exécuter |
| `tools.read_prefixes` | Préfixes identifiant une lecture. Tout le reste est une écriture |
| `tools.trust_read_only_hint` | Autorise l'annotation du serveur à dispenser un outil de confirmation |
| `tools.require_rfc3339` | Paramètres devant être des dates avec fuseau explicite |
| `tools.dedupe_writes` | Écarte deux écritures identiques proposées dans le même tour |

Les valeurs par défaut donnent un service en lecture seule : sans `resource`,
rien n'est injecté ; sans `confirm_writes`, tous les outils s'exécutent
directement. C'est le réglage d'`internet-search` dans l'exemple livré.

`permission_domain` devient obligatoire dès que `confirm_writes` est vrai.
Sans lui, l'application ne saurait pas quelle permission exiger avant
d'exécuter une action confirmée, et écrirait donc sans contrôle
d'autorisation.

### Classer les outils en lecture ou en écriture

Deux signaux servent à décider si un outil exige une confirmation.

L'annotation `readOnlyHint` du protocole MCP, quand le serveur la fournit,
est écoutée **de façon asymétrique** :

- « cet outil écrit » est toujours cru, même si le nom commence par un
  préfixe de lecture. Un serveur qui se déclare dangereux ne gagne rien à
  mentir, et le croire ne coûte qu'une confirmation ;
- « cet outil ne fait que lire » est ignoré par défaut. C'est le serveur qui
  l'affirme sur lui-même, rien ne le vérifie : un serveur compromis
  annonçant une suppression comme lecture contournerait la confirmation,
  c'est-à-dire la garantie centrale du système. `trust_read_only_hint: true`
  lève cette réserve, serveur par serveur, pour ceux dont vous maîtrisez le
  code.

Les préfixes de nom prennent le relais quand le serveur n'annote rien. Ils
restent nécessaires, la plupart des serveurs n'annotant pas leurs outils.

Un point subtil du protocole : `readOnlyHint` est un booléen avec `omitempty`,
si bien qu'une annotation absente et un « cet outil écrit » se ressemblent une
fois sérialisés. Automata distingue les deux cas au niveau du bloc
d'annotations entier. Un serveur qui n'annote rien conserve donc le
comportement fondé sur les noms, au lieu de voir toutes ses lectures passer
par une confirmation.

Les en-têtes déclarés ici valent pour tout le monde. Pour donner à chaque
utilisateur son propre jeton, voir [agents.md](agents.md).

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
    - id: vector
      type: sqlitevec
      path: /data/memory-vec.sqlite
      client: embeddings
      weight: 1
  retrieval:
    profile: balanced
    client: small
  policies:
    private_can_write_org: false
    org_readable_by_children: true
  consolidation:
    enabled: true
    client: main
    cron: "40 4 * * *"
    min_memories: 10
    reflection:
      enabled: false
      min_episodes: 5
      retention_days: 0
```

Mémoire persistante, portée par Amoxtli. Le store garde le contenu, l'index
sert la recherche. Les deux doivent être sauvegardés ensemble : un index sans
store ne vaut rien, un store sans index rend les souvenirs introuvables
jusqu'à `automata memory reindex`.

Chaque entrée de `indexes` déclare un index : `bleve` (plein texte) ou
`sqlitevec` (sémantique, vecteurs). Un index `sqlitevec` exige `client`, une
entrée de `llm_clients` fournissant les embeddings (ex. provider `mistral`,
modèle `mistral-embed`) ; chaque souvenir indexé et chaque recherche coûtent
alors un appel d'embeddings. Déclarer les deux types donne une recherche
hybride, fusionnée selon `weight`.

`retrieval.profile` choisit le compromis coût/qualité de la recherche, calqué
sur les profils amoxtli : `fast` (défaut) n'ajoute aucun appel LLM ;
`balanced` active HyDE — le `client` déclaré (requis, un modèle économique
suffit) reformule chaque requête distincte en document hypothétique avant la
recherche, ce qui améliore nettement la recherche sémantique.

`private_can_write_org` à `true` autoriserait une conversation privée à écrire
dans la mémoire commune. Le plan l'interdit et la valeur par défaut le reflète.

`org_readable_by_children` laisse un rôle restreint lire la mémoire commune.

`consolidation` réorganise périodiquement les souvenirs pour qu'ils ne
s'accumulent pas sans limite : à la cadence `cron` (5 champs, heure locale du
serveur, `"40 4 * * *"` par défaut), chaque portée comptant au moins
`min_memories` souvenirs (10 par défaut) est soumise au `client` déclaré, qui
propose un plan de réorganisation — fusionner les souvenirs redondants en un
seul texte à jour, oublier les faits périmés ou sans valeur durable. Le plan
est appliqué avec des garde-fous : jamais de fusion entre portées
différentes, identifiants vérifiés, et au plus un tiers de la portée en
oublis secs par passe — un plan qui propose de vider la mémoire est refusé.
Désactivée par défaut.

La même passe peut produire au plus deux « insights » par portée : des
souvenirs de synthèse déduits d'un motif traversant plusieurs souvenirs
(« consulte la météo chaque matin »), écrits avec l'origine `reflection`
sans supprimer les originaux. Le prompt exige la parcimonie — aucun insight
au moindre doute — et le compteur `memory_insights` de `/metrics` les
mesure.

La dernière exécution est persistée (table `maintenance_runs`) : un
redémarrage ne relance pas la consolidation et n'en repousse pas l'échéance.
Le contenu des souvenirs transite vers le fournisseur du `client` déclaré
(comme pour toute recherche mémoire) mais n'est jamais journalisé. Le
compteur `memories_consolidated` de `/metrics` mesure les souvenirs
supprimés (fusionnés ou oubliés).

`reflection` ajoute à la même passe une réflexion épisodique : les épisodes
verbatim récents (`conversation.compaction.record_episodes`) de chaque
portée personnelle ou de groupe sont relus pour en dégager des habitudes ou
préférences récurrentes que personne n'a jamais énoncées — ce que ni
l'extraction de faits (un fragment à la fois) ni les insights (limités aux
faits déjà extraits) ne peuvent voir. Au plus deux motifs par portée et par
passe, formulés prudemment, exigeant plusieurs occurrences, mémorisés avec
l'origine `episode_reflection` ; les épisodes sont lus, jamais modifiés.
Une portée comptant moins de `min_episodes` épisodes nouveaux (5 par
défaut) attend d'accumuler davantage de matière ; au-delà de 20 épisodes,
la passe traite les plus anciens et rattrape le reste les nuits suivantes.
C'est la production de souvenirs la plus spéculative et la plus coûteuse en
tokens (du verbatim, pas des faits condensés) : désactivée par défaut.

`retention_days` (0 par défaut : conservation illimitée) purge les épisodes
plus vieux que cet âge, mais JAMAIS un épisode qu'aucune réflexion réussie
n'a couvert — consolider avant d'oublier. Les compteurs `episode_patterns`
et `episodes_purged` de `/metrics` mesurent la réflexion.

## conversation

```yaml
conversation:
  history_limit: 20
  compaction:
    enabled: true
    client: main
    max_summary_chars: 2000
    extract_facts: true
    record_episodes: true
    max_facts: 5
```

`history_limit` borne le nombre de messages passés rejoués au modèle à
chaque tour (20 par défaut). Au-delà, sans compaction, les messages sortent
simplement du contexte.

La compaction leur donne une seconde vie : quand une conversation dépasse le
double de la fenêtre, les messages excédentaires sont condensés en un résumé
roulant persisté (table `conversation_summaries`), réinjecté en tête de
contexte à chaque tour. L'assistant garde ainsi le fil — préférences émises,
décisions prises, demandes en cours — bien après que les messages exacts ont
quitté la fenêtre. Les messages couverts par le résumé ne sont plus rejoués
verbatim.

`client` référence l'entrée de `llm_clients` qui produit les résumés — un
modèle économique suffit, requis quand `enabled` est vrai. La compaction
s'exécute en tête de tour, environ une fois tous les `history_limit`
messages ; son échec n'est jamais bloquant, le tour continue avec le résumé
précédent. Le compteur `conversations_compacted` de `/metrics` la mesure.

Le résumé condense des messages : c'est du contenu privé, envoyé au
fournisseur du `client` déclaré et stocké dans la base applicative, jamais
journalisé.

`extract_facts` complète le résumé par la mémoire à long terme : à chaque
compaction, les messages condensés sont aussi passés au même `client` pour en
extraire les faits durables (préférences stables, décisions, engagements,
dates importantes), stockés dans la mémoire Amoxtli avec la portée de la
conversation — personnelle en privé, de groupe en groupe, jamais `org`. Au
plus `max_facts` faits par compaction (5 par défaut). Requiert un système de
mémoire configuré (section `memory`) ; un échec d'extraction n'est jamais
bloquant. Le compteur `memories_extracted` de `/metrics` la mesure. Combinée
à `memory.consolidation`, cette extraction forme le cycle complet de la
mémoire : les faits entrent au fil des compactions, la consolidation
nocturne les fusionne et purge les périmés.

`record_episodes` conserve en plus le fragment condensé VERBATIM dans la
mémoire épisodique (même store Amoxtli, hors de portée de la consolidation
et de `search_memory`), horodaté et étiqueté par nom affiché — jamais par
identifiant interne. C'est ce qui alimente l'outil
`search_conversation_history` (drapeau `memory.history` de l'agent) : quand
le résumé et les faits extraits n'ont pas retenu le détail d'une discussion
passée, l'agent peut retrouver ce qui a réellement été dit. Aucun appel LLM
supplémentaire : le fragment est enregistré tel quel. Le compteur
`episodes_recorded` de `/metrics` le mesure.

Le drapeau `memory.recall` d'un agent active le rappel automatique : à
chaque tour, une recherche mémoire sur le message entrant injecte jusqu'à
trois souvenirs pertinents (datés) dans le contexte, sans attendre que le
modèle pense à appeler `search_memory` — c'est ce qui fait qu'un assistant
« se souvient tout seul ». Le coût est une recherche par tour (plus un
appel HyDE si `memory.retrieval.profile` vaut `balanced`) ; jamais
bloquant, portées lisibles uniquement. Le compteur `memory_recalls` de
`/metrics` le mesure.

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
      orgs: [home, work]
```

Une permission s'écrit `<domaine>.<portée>.<action>`. La portée vaut
`personal`, `group` ou `org`. L'action vaut `read`, `write` ou `delete`. Le
domaine est libre : `memory`, `calendar`, `todo`, ou le vôtre.

`kind` vaut `human` ou `service`. Réservez `service` aux principaux utilisés
par les tâches planifiées, avec le strict minimum de permissions.

`orgs` liste les organisations auxquelles le principal appartient. Le champ
est facultatif tant qu'une seule organisation est déclarée — le principal
appartient alors à celle-ci — et **obligatoire au-delà** : hériter
silencieusement de toutes les organisations donnerait à un collègue l'accès à
la mémoire de la famille. Les rôles, eux, restent globaux au principal.

Le chargement refuse un canal dont l'`org_id` est inconnu, ainsi qu'un membre
ou un `principal_id` qui n'appartient pas à l'organisation du canal : sans
cette vérification, la faute ne se manifesterait qu'au premier message reçu,
sous la forme d'un refus d'autorisation sans rapport visible avec sa cause.

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

La mention porte sur la rafale, pas sur le seul message : une mention écrite
par quelqu'un adresse aussi ses messages voisins immédiats. L'assistant
patiente une quinzaine de secondes après un média de groupe non adressé pour
laisser le temps de taper cette mention ; ce sursis ne s'ouvre que dans ce
cas précis, jamais en conversation privée ni pour un simple texte.

**Les vocaux ont leur propre règle.** Un message audio ne peut porter aucune
mention — sur WhatsApp, il n'a pas de légende. Deux gestes le rendent
adressé :

- **prononcer le nom de l'assistant dans le vocal** (« Automata, quel temps
  fera-t-il samedi ? ») : chaque vocal du groupe est transcrit et le nom y
  est cherché, sans tenir compte de la casse. Absent, le tour s'arrête là —
  aucune réponse, rien en base, aucun indicateur de saisie : le vocal a été
  écouté puis oublié ;
- **écrire « @assistant » juste après le vocal** : la mention voisine vaut,
  et les deux messages forment un seul tour.

Le premier geste a un coût et une implication à connaître : **tout vocal du
groupe est transcrit**, y compris ceux qui ne s'adressent pas à l'assistant
(la transcription des non-adressés est immédiatement jetée, jamais
persistée, jamais soumise au modèle de conversation). C'est le prix d'une
mention orale ; la transcription est facturée à l'organisation comme les
autres. Le nom cherché est le nom affiché du compte de l'assistant.

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

## web

Serveur web d'administration et de profil.
Désactivé par défaut ; lorsqu'il est activé, il démarre dans le même
processus que le worker, sur sa propre adresse d'écoute (à exposer derrière
un reverse proxy TLS).

```yaml
web:
  enabled: true
  addr: "127.0.0.1:8081"
  base_url: "https://automata.exemple.fr"   # compose les liens de profil /p/<id>
  session_secret: ${WEB_SESSION_SECRET}     # ≥ 32 octets, signe les cookies
  admin:
    email: "operateur@exemple.fr"
    password_hash: ${WEB_ADMIN_PASSWORD_HASH}   # produit par `automata web hash-password`
  mail_provider: ""       # optionnel : provider courier "mail" pour les codes de vérification
  credits:
    usd_per_credit: 0.001 # taux provisoire d'affichage de la conso en crédits
    packs:                # offres affichées sur la page Crédits du profil
      - { credits: 1000, price_eur: 9 }
      - { credits: 4400, price_eur: 35, featured: true }
      - { credits: 12000, price_eur: 89 }
```

Écrans servis : connexion opérateur (`/admin/login`, session cookie signée
12 h, 5 tentatives par quart d'heure), organisations (liste, détail avec
portefeuille de crédits, gestes commerciaux, bascule « organisation
offerte »), comptes membres avec **jeton de liaison affiché une seule
fois** (seul le SHA-256 est stocké), canaux et plateformes, alertes
d'exploitation, et les pages de profil ouvertes par **lien temporaire à
usage unique** (15 minutes, `/p/<id>.<secret>`).

Le profil d'un membre regroupe ce qui lui appartient : adresse de secours,
crédits et consommation, **« Ce que je retiens »** — ses souvenirs
personnels, mot pour mot, à corriger ou effacer un par un (jamais ceux d'un
groupe, qui appartiennent au groupe) —, **« Suggestions »** (voir la
section `introspection`), **« Découvrir »** (des phrases à recopier pour
essayer chaque capacité), confidentialité (export et suppression), et un
onglet par plugin actif doté d'une interface.

Le serveur web ne travaille pas seul : un expéditeur
inconnu dont le message porte un jeton `atm_…` valide se rattache
lui-même — le membre pré-créé prend son identité de messagerie, son canal
privé (ou le groupe, pour un jeton de groupe) est lié à l'organisation, et
l'assistant répond par un mot de bienvenue rédigé par l'application, jamais
par le modèle. Les tenants ainsi enregistrés sont ensuite résolus depuis la
base : la configuration YAML reste prioritaire, la base prend le relais pour
ce qu'elle ne connaît pas. Les membres en ligne n'ont pas de rôle
configurable ; leurs permissions découlent de leur rôle produit
(`member`, `owner`, `readonly` — voir `identity.DynamicRolePermissions`).

Ce jeu couvre les trois domaines applicatifs — `memory`, `reminder` et
`task` — sur les portées personnelle et de groupe. Un `readonly` lit sans
écrire ; un `owner` gagne la vue d'organisation et la suppression des
souvenirs du groupe.

**Qui peut poser une échéance peut la retirer** : `reminder.group.delete` et
`task.group.delete` accompagnent les droits d'écriture correspondants. Les
réserver au propriétaire enfermait la personne dans son erreur — une tâche
hebdomadaire programmée à la mauvaise heure, reprogrammée, et la première
impossible à annuler. Le garde-fou n'est pas le rôle mais la portée : une
échéance n'est visible et annulable que depuis la conversation où elle a été
posée. `memory.group.delete` reste réservé au propriétaire, la différence
tenant à ce qu'un souvenir de groupe est du contenu accumulé, là où une
échéance est un mécanisme qu'on vient de poser. **Un domaine oublié dans ce jeu est irréparable en ligne** :
la configuration des rôles a migré en base, plus rien ne permet de
l'accorder à la main. L'agent proposerait alors un outil dont le refus ne
peut être levé par personne, exploitant compris.

Les agents déclarant `reminders: true` exposent aussi
`list_recent_activity` : le journal de ce qui a réellement été délivré ou
exécuté dans la conversation — rappels envoyés ou en échec, tâches
planifiées, plans d'actions confirmés. Il complète `list_reminders`, qui
ne montre que ce qui reste à venir : sans lui, un assistant interrogé sur
un rappel déjà reçu ne voyait qu'une liste vide et en concluait qu'il
n'avait jamais rien programmé, contredisant l'utilisateur qui venait
précisément de le recevoir.

Les agents déclarant `profile_link: true` exposent l'outil
`open_profile_link` : l'assistant ouvre lui-même à son interlocuteur un lien
temporaire vers sa page de profil, ce qui évite tout mot de passe côté
utilisateur final.

```yaml
agents:
  main:
    profile_link: true
```

### Comptes de messagerie

Depuis le pilier 2, les comptes de messagerie vivent **en base** et se
gèrent depuis l'écran « Canaux et plateformes » : ajout, arrêt, remise en
route et retrait, sans redémarrage du processus. Les comptes encore
déclarés dans `courier.providers` sont importés automatiquement au premier
démarrage, **configuration reprise à l'identique** — le chemin de session
WhatsApp compris, de sorte qu'aucun ré-appairage n'est nécessaire. Une fois
importés, ils ne sont plus relus depuis le YAML : la base fait foi.

La configuration de chaque compte est **chiffrée au repos**
(AES-256-GCM, clé dérivée de `web.session_secret` par HKDF) : elle porte
des mots de passe et des jetons d'accès. Changer `web.session_secret` rend
ces configurations illisibles, et les comptes concernés doivent alors être
ressaisis.

L'écran affiche l'état réel de chaque compte — connectée, appairage
requis, arrêtée, déconnectée — et, pour un compte WhatsApp non encore lié,
**le QR code à scanner directement dans le navigateur** (il n'est plus
imprimé dans les journaux du worker). Cela repose sur l'option
`whatsapp.WithQRHandler` de go-courier.

### Personnalisation par organisation

L'onglet « Personnalisation » d'une organisation règle ce qui distingue un
forfait d'un autre, sans toucher au fichier de configuration ni redémarrer
le service :

- une **consigne ajoutée** au prompt de l'assistant — après les règles de
  l'instance, jamais à leur place : une organisation précise le ton ou le
  contexte, elle ne s'accorde aucun droit ;
- les **spécialistes disponibles** : en retirer un le rend invisible de
  l'assistant pour cette organisation, qui ne pourra plus lui déléguer ;
- un **plafond d'appels d'outils** par tour, qui ne peut qu'abaisser celui
  de l'agent.

Ces réglages sont relus à chaque tour de conversation. Une lecture en
échec donne un tour aux réglages par défaut, jamais un tour raté.

L'écran **Paramètres d'instance** montre en lecture ce qui tourne
réellement — agents, modèles, serveurs d'outils, services de fond — sans
jamais afficher une clé d'API. La configuration technique reste dans le
fichier YAML : la modifier demande de l'éditer puis de redémarrer.

### Facturation

Dès que le serveur web est activé, la consommation mesurée
(`usage_records`) est convertie en débits de crédits toutes les dix
minutes (`internal/billing`) et inscrite au portefeuille de chaque
organisation, avec un libellé lisible par le client (« Usage —
conversations », « Usage — génération d'images », « Usage — recherche »,
« Usage — notes vocales »). Le premier passage ne fait que poser la borne
temporelle : activer la facturation ne débite jamais rétroactivement. Une
organisation absente des tables SaaS n'est facturée à personne.

Les organisations offertes sont remises à niveau une fois par mois civil :
le solde est complété jusqu'à l'allocation, jamais cumulé d'un mois sur
l'autre.

**Avant la coupure**, l'organisation est prévenue dans sa conversation
dès que son solde passe sous 15 % de son dernier apport : un message
rédigé par l'application — jamais par le modèle, une alerte de solde doit
être exacte — accompagné d'un lien de recharge si le destinataire est
identifié. L'alerte part une seule fois par descente ; une recharge
réarme le mécanisme. Les organisations offertes n'en reçoivent pas :
elles n'ont rien à recharger. Le destinataire est le responsable de
l'organisation quand il y en a un, sinon un membre rattaché, sinon un
canal de l'organisation (et le lien de profil est alors omis : il ouvre
un compte, il n'a rien à faire dans un groupe).

À solde épuisé, le service se met en pause : la conversation reçoit une
explication et un lien de recharge, sans appel au modèle, au plus une fois
par heure et par conversation. Une organisation sans aucun mouvement de
portefeuille (instance non facturée) n'est jamais mise en pause.

Depuis les écrans **Tarification** et **Consommation**, l'économie de la
monnaie virtuelle se pilote sans redémarrage : les offres de crédits, le
coût couvert par un crédit, les crédits de bienvenue et l'allocation par
défaut vivent en base et priment sur `web.credits`. Tant qu'aucune offre
n'a été créée en ligne, celles de la configuration font foi.

Deux réglages fixent l'économie du produit et se lisent ensemble : le
**coût couvert par un crédit** (`usd_per_credit`) dit ce qu'un crédit doit
payer d'inférence, et la **marge visée** (`target_margin`, 60 % par
défaut) dit ce qu'il doit rapporter en plus. L'écran calcule alors, pour
chaque offre, sa marge réelle et le prix qui atteindrait la cible — et
signale en rouge toute offre **vendue à perte**, avant qu'elle ne soit
proposée aux clients plutôt qu'à la fin du mois.

La marge visée n'est pas une contrainte : elle n'empêche pas de publier un
tarif d'appel. Prévoyez-y de la place, car tout ne se facture pas — les
crédits offerts, les coûts qu'un fournisseur ne rapporte pas, les appels
échoués pèsent sur le résultat sans être payés par personne.

L'écran de tarification affiche surtout la seule mesure qui compte pour
l'exploitant : **crédits vendus contre coût réel** sur le mois, avec la
marge estimée et le nombre d'appels dont le fournisseur n'a rapporté aucun
coût — la marge est optimiste d'autant. La conversion des dollars en euros
utilise un taux configurable ; c'est un ordre de grandeur, pas une
conversion comptable.

**Repli tarifaire.** Tous les fournisseurs ne rapportent pas le coût de
leurs appels. Sans filet, un tel appel serait enregistré à zéro et
décompté zéro crédit : la consommation partirait en fuite. Le coût est
donc estimé à l'enregistrement, à partir des tokens et d'une grille
tarifaire par modèle (dollars par million de tokens), réglable dans
l'écran de tarification. Une entrée partielle couvre une famille entière
(`deepseek/`), et les modèles absents de la grille retombent sur des
tarifs de repli volontairement supérieurs aux modèles économiques : une
surestimation se voit et se corrige, une sous-estimation disparaît.
L'estimation n'est jamais présentée comme une mesure — `cost_reported`
reste faux, et les écrans distinguent les deux.

Pour les traces déjà enregistrées à zéro avant la mise en place de la
grille :

```
automata usage reprice -config config.yaml
```

L'écran de consommation croise les traces d'usage selon les dimensions
voulues (organisation, membre, agent, modèle, nature, fournisseur, jour,
mois), sur la période choisie, et s'exporte en CSV avec les mêmes filtres.

Le paiement en ligne s'active en renseignant les deux secrets Stripe —
l'un sans l'autre est refusé au chargement (une session de paiement dont
le résultat ne pourrait pas être crédité ferait payer un client pour
rien) :

```yaml
web:
  stripe:
    secret_key: ${STRIPE_SECRET_KEY}       # sk_…
    webhook_secret: ${STRIPE_WEBHOOK_SECRET} # whsec_…
    tax_code: "txcd_10000000"                # facultatif
```

Le `tax_code` classe fiscalement les crédits vendus. Stripe l'exige dès
que Stripe Tax est activé sur le compte : le produit est créé à la volée
au moment du paiement, il n'existe pas de catalogue où le déclarer une
fois pour toutes. Le défaut, `txcd_10000000`, correspond aux services
fournis par voie électronique — ce que vend Automata. Un régime
particulier (logiciel professionnel en abonnement, par exemple) se règle
ici, mais le choix du code relève du comptable, pas du logiciel.

Les prix des offres sont hors taxes : Stripe ajoute la TVA applicable au
moment du paiement, et la page de crédits l'annonce (« 35 € HT »). La
recette inscrite au portefeuille est le prix hors taxes — la marge se
calcule donc sur ce qui revient réellement à l'exploitant.

Un achat réglé est confirmé dans la conversation privée de l'acheteur :
le message part du service, jamais du modèle, et n'est envoyé qu'une fois
même si Stripe rejoue l'événement. Un membre non rattaché à une
messagerie n'en reçoit pas — sa confirmation reste sur l'écran de retour.

Le retour de paiement ne vise jamais le lien de profil d'origine : celui-ci
est à usage unique et déjà consommé au moment où Stripe renvoie le client.
Un lien de retour neuf, valable une heure, est émis à l'ouverture de la
session de paiement — sans quoi le client verrait « ce lien a déjà servi »
juste après avoir payé.

Le point de réception des événements est `POST /stripe/webhook` (à
exposer publiquement, contrairement au reste de l'interface) : la
signature est vérifiée avec une tolérance de cinq minutes, et le crédit
est idempotent — l'identifiant de session est unique en base, un
événement rejoué ne crédite jamais deux fois. Sans ces secrets, les
boutons d'achat restent visibles mais inertes.

**Ma consommation.** La page « Ma consommation » du profil montre l'usage
du mois en catégories parlantes (conversations, recherches, images, notes
vocales) et son évolution sur six mois, en crédits — le mot « token »
n'y apparaît jamais. Les chiffres couvrent l'organisation entière, ce que
la page dit explicitement : les crédits sont partagés.

**Confidentialité (RGPD).** La page de profil « Confidentialité » liste ce
qu'Automata conserve d'une personne, et ouvre deux droits : télécharger
ses données (JSON lisible, en français) et les faire effacer. La
suppression exige d'écrire `SUPPRIMER` — elle est irréversible.

Trois règles gouvernent la suppression, et méritent d'être connues avant
de répondre à une demande :

- **les conversations de groupe ne sont pas effacées** : elles appartiennent
  aussi aux autres participants ;
- **le compte survit sous une identité neutre**, détaché de sa messagerie —
  le supprimer romprait les groupes auxquels la personne a participé ;
- **les traces de consommation sont conservées mais dissociées** (le
  principal disparaît, les montants restent) : ce sont des pièces
  comptables.

Sont effacés : les messages des conversations privées et leurs résumés,
les souvenirs de portée personnelle, les rappels, les jetons et liens de
profil, la liaison de canal privé et l'adresse de récupération.

Sous-commandes associées :

```
echo -n 'mot-de-passe' | automata web hash-password
automata web bootstrap -config config.yaml   # importe orgs et membres de la config (idempotent)
```

Développement des vues : `make web-tools` installe templ, templui et le
binaire Tailwind sous `tools/` ; `make web-generate` régénère les fichiers
`*_templ.go` et `internal/web/assets/app.css`. Les fichiers générés sont
commités : `go build` reste autonome.

## plugins

```yaml
plugins:
  enabled: true
  dir: ./plugins            # binaires exécutables, relatif au fichier de config
  client: main              # clé de llm_clients servant les sous-agents
  restart_cooldown: 30s     # facultatif
  mem_limit: ""             # GOMEMLIMIT des sous-processus, facultatif
  triggers:
    max_per_minute: 6       # par (plugin, organisation)
    max_concurrent: 2       # exécutions de sous-agents simultanées
```

Les plugins étendent Automata sans toucher à son cœur : chaque binaire
exécutable du répertoire est lancé en sous-processus (hashicorp/go-plugin,
gRPC) et peut fournir un **sous-agent** délégué, des **déclencheurs**
extérieurs (courriel entrant, alerte d'infrastructure) et sa propre
**interface**, rendue en iframe dans l'administration et la page de profil.
Le SDK des auteurs de plugins est le module `pkg/pluginsdk` ; le plugin
`plugins/email` sert d'exemple complet.

**L'activation se décide par organisation**, sur la fiche de l'organisation
(onglet Plugins). Elle est relue à chaque tour : une désactivation
s'applique au message suivant. Activer un plugin accorde aux membres de
l'organisation les permissions de son domaine (`email.personal.write`…) —
mais la porte des écritures reste la **confirmation humaine** : tout outil
de plugin non marqué lecture seule produit une action à confirmer par un
« confirmer » littéral dans la conversation, jamais une exécution directe.
Aucun réglage, du membre, de l'administrateur ou du plugin, ne peut
débrayer ce passage.

Ce que l'hôte garantit, quel que soit le plugin :

- les appels LLM des sous-agents passent par le client de l'instance
  (`plugins.client`) — comptabilité d'usage et débit de crédits inchangés ;
- l'identité d'un appel est construite par l'hôte, jamais par le modèle ni
  par le plugin ; un événement de déclencheur *désigne* une organisation et
  un membre, l'hôte re-vérifie l'activation et l'appartenance avant d'agir ;
- chaque connexion de service hôte est liée au plugin qui l'a établie : un
  plugin ne lit jamais les configurations ni les secrets d'un autre ;
- configurations et secrets des plugins sont scellés au repos (contexte de
  clé dédié) ; l'interface ne relit jamais une valeur de secret ;
- l'interface d'un plugin est servie par un port local fermé (jeton connu
  du seul reverse proxy) et rendue dans une iframe *sandbox* sans
  `allow-same-origin` : le document du plugin n'accède ni aux cookies ni au
  DOM de l'application. Cette origine opaque a une conséquence à connaître :
  le navigateur traite le cadre comme tiers et ne lui transmet aucun cookie.
  Les requêtes de l'iframe sont donc authentifiées par un **jeton signé
  porté par le chemin** (`/plugin-ui/<jeton>/…`), émis au rendu de la page
  pour une vue, une organisation, un membre et un plugin précis, et valable
  une heure. Un jeton ne décide de rien : l'activation du plugin et
  l'existence de l'organisation sont revérifiées à chaque requête, si bien
  qu'une désactivation coupe l'interface immédiatement ;
- les rafales de déclencheurs sont bornées (`triggers.*`) — abandon compté,
  jamais de file illimitée.

Une route publique, `GET /plugins/{nom}/oauth/callback`, est réservée aux
retours d'autorisation OAuth des plugins (le plugin `email` s'en sert pour
Gmail). Elle est délibérément étroite : seul ce chemin est proxifié, elle
ne transporte aucune identité, et c'est au plugin de prouver l'origine de
l'appel — le plugin `email` le fait par un paramètre `state` signé avec une
graine propre au membre.

Un plugin qui meurt est relancé au prochain usage, après le délai de
refroidissement ; le bouton « Redémarrer » de l'écran Plugins force la
relance. Le nom d'un plugin (déclaré par son descripteur) devient l'outil
`delegate_to_<nom>` : une collision avec un agent configuré est refusée au
chargement.

## introspection

```yaml
introspection:
  enabled: false
  cron: "20 5 * * 1"
  digest_cron: "50 5 1 * *"
```

Chaque semaine, Automata relit pour chaque membre rattaché ses **frictions**
des trente derniers jours — plans d'actions proposés jamais confirmés ou en
échec, rappels et tâches en échec — et les **habitudes** que la réflexion
épisodique a déjà rangées en mémoire, et en tire **au plus une** suggestion
d'amélioration : automatiser un geste répété, activer une capacité
inutilisée, corriger ce qui échoue. La suggestion attend sur l'onglet
« Suggestions » du profil ; seule une friction nette est poussée en
conversation, au plus une par passe.

Ce que le modèle reçoit est un dossier de métadonnées (outil, permission,
statut, date) et de textes déjà produits par des modèles — **jamais un
verbatim de conversation**. Un dossier vide ne déclenche aucun appel. Une
suggestion écartée par la personne est montrée au modèle avec ce sort, et
ne revient pas. « Ne plus rien me proposer », sur la même page, coupe tout,
collecte comprise.

Le modèle se règle en ligne (rôle `introspection`). L'ancrage suit la règle
de la consolidation : un déploiement ne déclenche jamais de passe, et un
nouveau membre attend sa première échéance.

`digest_cron` envoie à l'exploitant désigné (écran « Alertes ») une synthèse
mensuelle des frictions par type et par domaine, et des suggestions émises,
suivies et écartées — **des comptes seulement**, jamais un nom ni un contenu.

## backup

Sauvegardes périodiques des bases SQLite. Désactivées par défaut ; une
instance qui sert des clients ne devrait jamais tourner sans.

```yaml
backup:
  enabled: true
  directory: ./backups   # relatif au fichier de configuration
  interval: 6h           # défaut : 6h
  keep: 10               # copies conservées par base, défaut : 10
  extra_paths:
    whatsapp-session: ./whatsapp/data/courier/whatsapp?_foreign_keys=on
```

La base applicative (`storage.application`) et la mémoire
(`memory.store`) sont sauvegardées automatiquement ; `extra_paths` ajoute
les bases annexes — la **session de messagerie** en particulier, sans
laquelle une restauration coûterait un ré-appairage de chaque compte.

La copie se fait par `VACUUM INTO`, qui produit une base cohérente pendant
que le service écrit : copier le fichier à chaud donnerait une sauvegarde
corrompue en mode WAL. Chaque copie est d'abord écrite sous un nom
temporaire puis renommée — une copie partielle ne doit jamais pouvoir
passer pour une sauvegarde valide — et reçoit les permissions `0600`
(le répertoire, `0700`) : elle porte les mêmes données personnelles que
l'original.

Une base absente est ignorée sans erreur, et l'échec d'une source
n'empêche pas les autres d'être sauvegardées.

## Comptabilité d'usage

Aucune clé de configuration : dès que le stockage applicatif est présent
(`storage.application`), chaque appel d'inférence réussi (complétion,
transcription, génération d'image) laisse une trace comptable dans la table
`usage_records` — organisation, principal, conversation, composant
(`agent`, `compaction`, `consolidation`, `transcription`), agent, provider,
modèle, volumes de tokens et, quand le provider le rapporte, le coût
réellement facturé (OpenRouter le fait, en USD). Aucun contenu n'est jamais
stocké : uniquement des identifiants, des comptes et des montants, dans
l'optique d'une refacturation de l'accès par organisation/utilisateur.

Règles d'attribution :

- un tour interactif est facturé au principal qui l'a déclenché ; un appel
  fait par un spécialiste délégué est attribué au spécialiste (colonne
  `agent`), pas à l'orchestrateur ;
- les tâches de fond (compaction, consolidation) sont facturées à
  l'organisation, avec `principal_id` vide ;
- un appel non attribuable est enregistré orphelin (champs vides) plutôt
  qu'ignoré.

Limites connues : les embeddings de l'index sémantique (`sqlitevec`) ne
sont pas comptabilisés — leurs appels partent de l'indexation asynchrone
d'amoxtli, hors de tout contexte de requête — et les providers autres
qu'OpenRouter ne rapportent pas de coût : seuls leurs tokens sont
enregistrés (`cost_reported = 0`), la colonne `appels sans coût` du rapport
signale ces trous.

Consultation :

```
automata usage report -config config.yaml \
    -from 2026-08-01 -to 2026-09-01 \
    -group-by org,principal
```

Par défaut : le mois civil courant, agrégé par `org`. Dimensions
acceptées : `org`, `principal`, `conversation`, `component`, `agent`,
`kind`, `provider`, `model`, `day`, `month`. Les compteurs `usage_records`
et `usage_record_failures` de l'export `/metrics` suivent l'enregistrement
lui-même (un compteur d'échecs qui monte signale des coûts perdus pour la
comptabilité).

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
