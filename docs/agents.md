# Agents, spécialistes et serveurs MCP

Ce document explique comment ajouter vos propres sous-agents, leur brancher
des serveurs MCP, et faire en sorte que chaque utilisateur se connecte à ces
serveurs avec ses propres identifiants.

## Comment les agents sont organisés

Un orchestrateur parle à l'utilisateur. Il ne connaît aucun outil métier. Il
dispose seulement d'outils conceptuels : déléguer à tel spécialiste, chercher
en mémoire, y écrire, y supprimer.

Un spécialiste ne parle jamais à l'utilisateur. Il reçoit un objectif de
l'orchestrateur, travaille avec ses propres serveurs MCP, et rend un résumé.

Cette séparation a une raison précise. Charger tous les schémas d'outils dans
un seul agent noierait le modèle, coûterait cher à chaque tour, et donnerait à
un agent généraliste le pouvoir d'appeler n'importe quoi. Un spécialiste ne
voit que ses propres outils, et rien de ce que voient les autres.

```text
utilisateur
   │
   ▼
orchestrateur "main"          outils : delegate_to_*, search_memory, remember, forget_memory
   │
   ├── spécialiste "agenda"   outils : ceux du serveur MCP google-calendar
   ├── spécialiste "research" outils : ceux du serveur MCP internet-search
   └── spécialiste "todo"     outils : ceux du serveur MCP todo
```

## Ce que reçoit un agent

Son prompt système est composé par l'application, dans cet ordre :

1. Les règles de sécurité invariantes. Codées en dur, jamais issues de la
   configuration, impossibles à désactiver.
2. Votre contenu, sous le titre « Personnalité et mission ».
3. Ses capacités effectives, listées à partir de `capabilities`.

Puis, à chaque requête, un bloc de contexte séparé : nom de l'agent,
organisation, portée d'exécution, type de canal.

Vos fichiers de prompt ne portent donc que la personnalité et la mission.
N'y écrivez pas de règles de sécurité. Elles y seraient redondantes, et
surtout elles donneraient l'illusion qu'une règle de sécurité peut se
configurer. Ce n'est pas le cas : aucune décision d'autorisation ne dépend du
texte d'un prompt.

Un spécialiste ne reçoit pas l'historique de la conversation principale. Il
obtient l'objectif, les éléments que l'orchestrateur a jugé utiles, la portée
résolue et les pièces jointes du tour. Les pièces jointes font exception à la
règle d'isolation parce qu'un modèle ne peut pas recopier une image dans une
chaîne de caractères : sans cela, « lis cette affiche et crée le rendez-vous »
serait impossible.

## Ajouter un spécialiste

Aucun domaine n'est connu du code. Agenda, tâches, météo, domotique, comptabilité :
tous se déclarent de la même façon, et l'application applique le même
mécanisme. Ajouter un spécialiste ne demande jamais de modifier du Go.

Prenons un suivi de ruches, pour bien montrer qu'aucun domaine n'est
privilégié.

### 1. Déclarer le serveur MCP et sa politique

```yaml
mcp_servers:
  ruches:
    transport: http
    url: ${RUCHES_MCP_URL}
    headers:
      Authorization: Bearer ${RUCHES_MCP_TOKEN}

    # L'application injecte cet identifiant dans chaque appel, en le lisant
    # dans channels[].resources.apiary selon la portée de la conversation.
    resource:
      key: apiary
      parameter: apiary_id

    # Les permissions exigées seront apiary.<portée>.write.
    permission_domain: apiary

    tools:
      # Les écritures deviennent des actions à confirmer.
      confirm_writes: true
      read_prefixes: [list_, get_]
      dedupe_writes: true
      # Si votre serveur annote correctement ses outils (readOnlyHint) et que
      # vous en maîtrisez le code, ceci remplace la convention de nommage :
      # trust_read_only_hint: true
```

Puis les ressources correspondantes sur les canaux :

```yaml
channels:
  - provider: whatsapp
    channel_id: ${ALICE_PRIVATE_CHANNEL_ID}
    kind: private
    scope: personal
    scope_id: alice
    principal_id: alice
    resources:
      apiary: ${ALICE_APIARY_ID}
```

À partir de là, `list_hives` s'exécute directement avec le bon `apiary_id`, et
`register_harvest` produit une action que l'utilisateur devra confirmer, sous
la permission `apiary.personal.write`. Sans écrire de code.

Un service en lecture seule se déclare sans rien de tout cela :

```yaml
mcp_servers:
  internet-search:
    transport: http
    url: ${SEARCH_MCP_URL}
```

Tous ses outils s'exécutent directement, aucune ressource n'est injectée.

Seul le transport `http` existe. Un serveur lancé en sous-processus (`stdio`)
n'est pas représentable.

### 2. Écrire son prompt

`prompts/ruches.md` :

```markdown
Tu es le spécialiste du rucher. Tu n'échanges pas avec l'utilisateur : tu
reçois un objectif de l'agent généraliste et tu lui rends un résultat.

## Ton, personnalité

Bref et factuel. Poids en kilogrammes, jamais de longue narration.

## Ta mission

Consulter l'état des ruches et enregistrer les récoltes.

Si une ruche est désignée de façon ambiguë, tu demandes une précision plutôt
que de choisir. Tu n'inventes jamais un relevé que la source ne donne pas.
```

### 3. Déclarer l'agent

```yaml
agents:
  ruches:
    type: specialist
    client: main
    system_prompt:
      file: ../prompts/ruches.md
    mcp_servers:
      - ruches
    limits:
      max_sequential_tool_calls: 4
      max_actions_per_turn: 3
      tool_timeout: 20s
      max_tool_result_bytes: 8KiB
      max_tool_context_bytes: 16KiB
```

Les cinq limites sont obligatoires. Une valeur nulle ou négative fait échouer
la validation.

### 4. Le rendre joignable

```yaml
agents:
  main:
    delegates:
      - agenda
      - research
      - todo
      - ruches
```

L'orchestrateur expose alors un outil `delegate_to_ruches`. Un spécialiste
absent de `delegates` n'est jamais atteignable.

### 5. Valider

```bash
automata config validate -config config/config.yaml
```

La validation vérifie que chaque délégué existe, que chaque serveur MCP
référencé est déclaré, qu'aucun cycle de délégation ne se forme, et que le
fichier de prompt est lisible.

## Sessions MCP

Le gestionnaire ouvre une connexion par couple (session, serveur), et la
réutilise ensuite. La clé de session est l'identifiant de conversation, soit
`<provider>:<channel_id>`.

Deux conversations n'obtiennent donc jamais la même connexion. Cela compte
pour les serveurs qui gardent un état entre les appels : le contexte d'un
canal ne peut pas fuir dans un autre.

Une connexion vit aussi longtemps que le processus, pas le temps d'un message.
Une action proposée dans un message et confirmée dans un autre réutilise la
même connexion.

Les connexions se ferment à l'arrêt du processus, ou explicitement pour une
session donnée.

## Donner à chaque utilisateur ses propres identifiants

Les en-têtes déclarés sous `mcp_servers` valent pour tout le monde. Cela
convient à un serveur de recherche web. Cela ne convient pas à un agenda : si
Alice et Léo partagent le même jeton, Léo lit l'agenda d'Alice.

Déclarez alors la connexion au niveau du principal :

```yaml
identities:
  principals:
    - id: alice
      kind: human
      display_name: Alice
      roles: [adult]
      mcp:
        google-calendar:
          headers:
            Authorization: Bearer ${ALICE_GCAL_TOKEN}

    - id: leo
      kind: human
      display_name: Léo
      roles: [child]
      mcp:
        google-calendar:
          headers:
            Authorization: Bearer ${LEO_GCAL_TOKEN}
```

Ce qui se passe alors, quand un spécialiste appelle `google-calendar` :

- l'application lit le principal de l'identité d'exécution, résolue depuis
  `origins` avant tout appel au modèle ;
- si ce principal déclare une surcharge pour ce serveur, la connexion utilise
  ses en-têtes ;
- la clé de session inclut son identifiant, ce qui lui donne une connexion
  distincte.

Ce dernier point est ce qui rend le mécanisme sûr. Dans un canal de groupe,
Alice et Léo parlent dans la même conversation. Sans cette séparation, le
premier à déclencher un appel imposerait son jeton au second, qui lirait alors
l'agenda de quelqu'un d'autre. Chacun a donc sa connexion, y compris dans un
groupe partagé.

Un principal sans surcharge garde le comportement d'origine : une connexion
commune à la conversation, avec les en-têtes du serveur.

### Surcharger l'URL

```yaml
      mcp:
        crm:
          url: https://crm.example.test/tenants/alice/mcp
          headers:
            Authorization: Bearer ${ALICE_CRM_TOKEN}
```

`url` vide conserve celle du serveur. Les en-têtes du principal s'ajoutent à
ceux du serveur et l'emportent en cas de même nom, ce qui permet de ne
surcharger que l'autorisation.

### Patrons dans l'URL et les en-têtes http

Quand plusieurs principaux ne diffèrent que par une valeur (tenant, clé
d'API, jeton), déclarez le motif une fois côté serveur et seulement les
valeurs côté principal :

```yaml
mcp_servers:
  meteo:
    transport: http
    url: https://mcp.example.com/tenants/{{tenant}}/mcp?api_key={{api_key}}

identities:
  principals:
    - id: alice
      mcp:
        meteo:
          values:
            tenant: alice
            api_key: ${ALICE_METEO_KEY}
```

Les patrons `{{nom}}` sont résolus sur la configuration EFFECTIVE : si le
principal remplace l'URL du serveur par la sienne, ce sont les patrons de SA
propre URL qui doivent être couverts, ceux de l'URL du serveur ne comptent
plus. Un principal sans `values` pour un serveur à patrons n'y a pas accès —
le serveur n'est jamais appelé avec un patron littéral ni avec les valeurs
d'un autre. Préférez un en-tête à une clé en variable d'URL quand le serveur
le permet : une URL transite en clair dans les journaux de proxys et de
serveurs intermédiaires, un en-tête beaucoup plus rarement.

### Serveurs stdio : un processus par principal

Pour un serveur en transport `stdio` (voir
[configuration.md](configuration.md), `mcp_servers`), la surcharge ne porte
ni URL ni en-têtes mais des `values`, qui résolvent les patrons `{{nom}}`
déclarés dans `command` et `env` du serveur :

```yaml
identities:
  principals:
    - id: alice
      kind: human
      roles: [adult]
      mcp:
        imap:
          values:
            host: imap.example.com
            port: "993"
            user: alice@example.com
            password: ${ALICE_IMAP_PASSWORD}
```

Chaque principal surchargé obtient **son propre processus serveur**, lancé
avec ses valeurs — Alice interroge sa boîte mail, jamais celle de Léo, même
dans un groupe. Ce processus est partagé entre toutes les conversations du
même principal (la frontière de sécurité est le principal, pas la
conversation), ce qui borne le nombre de processus à un par couple
(principal, serveur). Un principal sans `values` pour un serveur à patrons
n'y a pas accès du tout : l'outil lui est refusé avec une erreur explicite,
sans repli possible sur les identifiants d'un autre.

Le cloisonnement par portée reste l'affaire des permissions : c'est
`mail.personal.read` (sans `mail.group.*`) qui empêche de demander la
lecture d'une boîte personnelle depuis un canal de groupe, pas le transport.

### Ce qui est vérifié au démarrage

Une surcharge visant un serveur inexistant est une erreur de validation, pas
un avertissement. Sans cela, le principal se rabattrait silencieusement sur le
jeton commun, donc potentiellement sur les ressources de quelqu'un d'autre.
Une surcharge vide est refusée pour la même raison. La validation vérifie
aussi la cohérence transport/surcharge (`url`/`headers` réservés à `http`,
`values` valides sur les deux transports dès que le serveur déclare des
patrons), que chaque surcharge couvre tous les patrons de la configuration
effective, et qu'aucune valeur ne reste sans patron correspondant — les
erreurs ne citent que les noms de patrons, jamais les valeurs.

### Ce que le mécanisme ne fait pas

Il ne gère pas un flux OAuth. Il n'y a ni rafraîchissement de jeton, ni
stockage de jeton par utilisateur : vous fournissez des jetons durables par
variable d'environnement, et vous les faites tourner vous-même. Un jeton
expiré produit une erreur MCP que le spécialiste remonte en clair.

Il ne s'applique pas aux tâches planifiées autrement que par leur principal de
service. Une tâche s'exécutant sous `scheduler-readonly` utilise les
surcharges de ce principal, s'il en a.

## Résolution des ressources

Un serveur qui déclare `resource` voit son identifiant injecté par
l'application, à partir de `channels[].resources` et de la portée de la
conversation. Une valeur fournie par le modèle sous ce nom est écartée.

En lecture, l'identifiant est injecté à l'appel. En écriture, l'outil
n'exécute rien : il enregistre une action à confirmer, dont les arguments ne
contiennent délibérément aucun identifiant de ressource. Celui-ci est résolu à
nouveau au moment de la confirmation, depuis la portée du plan. Une action
confirmée écrit donc toujours dans la ressource courante de sa portée.

Si la portée courante ne déclare pas la ressource, le spécialiste échoue avec
une erreur claire avant tout appel au modèle, plutôt qu'au moment de confirmer
un plan déjà annoncé à l'utilisateur.

## Écritures et confirmation

Un spécialiste ne réalise jamais une écriture externe dans le tour où il la
décide. Il produit une action, qui devient un plan persisté. L'utilisateur
répond « confirmer » ou « annuler » en toutes lettres. Ces deux mots sont
interceptés avant tout appel au modèle : le modèle ne peut pas se
confirmer lui-même.

À la confirmation, l'application recharge le plan, vérifie son état et son
expiration, contrôle qui confirme, **revérifie les permissions**, résout à
nouveau les ressources, puis exécute les actions une par une. Le rapport
distingue chaque succès et chaque échec.

La revérification des permissions surprend parfois : une action proposée peut
échouer à la confirmation si le rôle a changé entre-temps, ou si la permission
n'a jamais été accordée. C'est voulu. L'autorisation obtenue au moment de la
proposition ne vaut rien au moment d'écrire.

Un spécialiste qui déclare `calendar.personal.write` dans `capabilities` doit
aussi voir cette permission accordée au **rôle du principal**. Les deux sont
nécessaires : les capacités de l'agent et les permissions de l'utilisateur se
croisent.

## Choisir les limites

`max_sequential_tool_calls` doit couvrir le pire enchaînement légitime. Un
spécialiste agenda qui consulte les événements avant de proposer une création
a besoin d'au moins trois tours. Trop bas, le tour échoue au lieu d'aboutir.

`max_actions_per_turn` reflète ce que l'utilisateur peut relire d'un coup
avant de confirmer. Au-delà, le lot entier est rejeté avec un message
demandant de découper la demande.

`max_tool_result_bytes` et `max_tool_context_bytes` protègent votre facture.
Un serveur de recherche renvoyant des pages entières épuise le second bien
avant le premier.

## Déboguer un spécialiste

Les journaux MCP donnent l'essentiel sans exposer de contenu :

```json
{"level":"INFO","msg":"mcp: connexion établie","server":"meteo","session":"whatsapp:33612345678@s.whatsapp.net"}
{"level":"INFO","msg":"mcp: appel d'outil terminé","server":"meteo","tool":"forecast","duration":"120ms","status":"success","result_bytes":842,"truncated":false}
```

La session affichée indique quelle connexion a servi. Une session suffixée par
un identifiant de principal signale une connexion par utilisateur.

`truncated: true` veut dire que le modèle a reçu un résultat coupé. Il en est
informé, mais si cela arrive souvent, augmentez `max_tool_result_bytes` ou
demandez au serveur de répondre plus court.

Quelques symptômes fréquents :

Le spécialiste n'est jamais appelé. Vérifiez qu'il figure dans `delegates`, et
que son rôle est clair dans le prompt de l'orchestrateur. Un modèle ne délègue
pas à un agent dont il ne comprend pas l'utilité.

Le tour échoue avec un plafond atteint. `max_sequential_tool_calls` est trop
bas pour la tâche.

Une écriture reste sans effet. Elle attend une confirmation. Cherchez le plan
avec `automata admin inspect -kind plans`.

Un utilisateur voit les données d'un autre. Le serveur est déclaré sans
surcharge par principal : tout le monde partage le même jeton.
