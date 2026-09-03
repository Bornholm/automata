# Modèle de sécurité

Ce document dit contre quoi Automata se défend, où passent les frontières
de confiance dans le code, et ce qu'on a vérifié point par point. Il dit
aussi ce qu'on ne fait pas. Cette dernière partie est celle à lire si vous
n'en lisez qu'une.

Le chapitre 3 est une revue à date. Je l'ai faite avant le premier
déploiement réel et je la rejoue à chaque changement qui touche une
frontière. Un point "Conforme" veut dire qu'un test ou une lecture du code
l'a établi ce jour-là, pas que c'est vrai pour toujours.

## 1. Modèle de menace

Automata reçoit des entrées de trois origines non fiables :

1. Messages WhatsApp (et futurs canaux Go Courier), envoyés par :
   - des principaux connus (déclarés dans `identities.principals` /
     `origins`), dont l'identité applicative est résolue de façon fiable,
     mais dont le contenu du message reste un texte arbitraire ;
   - des principaux inconnus (aucune entrée `origins` correspondante),
     doivent être rejetés silencieusement, jamais traités ;
   - des membres de groupes non déclarés (aucun `channels` correspondant,
     ou canal déclaré mais expéditeur absent de `members`), même exigence de
     rejet.
   - Le provider Go Courier ne filtre rien lui-même (voir
     la doc de go-courier, "les providers ne filtrent rien,
     y compris les groupes non déclarés") : la robustesse est entièrement à
     la charge d'`internal/identity` et `internal/authorization`.
2. Serveurs MCP externes, potentiellement :
   - compromis : peuvent renvoyer un résultat d'outil contenant du texte
     conçu pour être réinjecté dans le contexte du LLM (injection de prompt
     indirecte) ;
   - lents ou indisponibles : doivent être bornés par un timeout et ne
     jamais bloquer indéfiniment le reste du système.
3. Contenu de message conçu pour manipuler le LLM (injection de prompt
   directe) : un message utilisateur peut littéralement demander à
   l'assistant d'"ignorer les règles précédentes", de changer de portée, de
   révéler des identifiants internes, ou de confirmer une action sans
   confirmation humaine réelle.

Dans les trois cas, l'hypothèse de travail est la même : **le LLM est un
composant non fiable dont la sortie (texte de réponse et arguments de
tool-call) ne doit jamais, par elle-même, constituer une décision de
sécurité.**

## 2. Frontières de confiance

### Ce que le LLM peut influencer

- Le texte de sa réponse à l'utilisateur.
- Le choix de déléguer à tel ou tel sous-agent conceptuel (`delegate_to_*`),
  parmi ceux explicitement proposés par l'application.
- Le choix d'appeler un outil et les arguments sémantiques de cet appel
  (le contenu d'une mémoire à retenir, le titre d'un événement, le texte
  d'une recherche).
- Le choix de proposer une action (qui reste soumise à confirmation humaine
  explicite et à revérification complète avant exécution, plan de conception, §10.5).

### Ce que le LLM ne peut JAMAIS influencer

- L'identité d'exécution (`model.ExecutionIdentity`) : résolue par
  `internal/identity.Resolver` à partir de l'origine réelle du message,
  avant tout appel au LLM.
- La portée (`personal`/`group`/`org`) et l'identifiant de portée
  (`ScopeID`) : dérivés de la conversation, jamais d'un paramètre de
  tool-call.
- Les permissions effectives : résolues par
  `internal/identity.EffectivePermissions` à partir de la configuration, et
  vérifiées par `internal/authorization.Authorizer.Authorize`, jamais par le
  contenu généré par le modèle.
- Les identifiants de ressources externes : résolus par
  `internal/resource.Resolve` à partir de la portée de la conversation et de
  la configuration, puis réinjectés de force dans les arguments d'outil. Toute
  valeur fournie par le modèle sous le paramètre déclaré est écrasée avant
  l'appel réel (voir §4, point A.2).
- L'autorisation d'une action sensible : une action proposée n'est
  jamais exécutée sur la seule foi de la proposition ; elle exige une
  confirmation humaine explicite (commande littérale `confirmer`/`annuler`,
  interceptée avant tout appel au LLM par
  `internal/conversation.Handler.Handle`, voir `action.ParseCommand`) puis
  une revérification complète des permissions au moment de l'exécution
  (`internal/action.Engine.executeAction`, étape 5 : "ne jamais se fier à
  l'autorisation obtenue lors de la proposition").

## 3. État constaté par point de vérification

### A.1. Frontières d'autorisation

**Conforme.** `authorization.Authorizer.Authorize` est appelé à chaque point
d'écriture/lecture sensible identifié :

- `internal/agent/memory_tools.go` : recherche mémoire
  (`searchAuthorizedScopes`), écriture (`newRememberTool`), suppression
  (`findByIDForDelete`), trois appels, un par opération.
- `internal/action/engine.go` (`executeAction`, étape 5) : revérification
  systématique des permissions au moment de la confirmation d'un plan
  d'actions, indépendamment de l'autorisation obtenue à la proposition.

Spécialistes MCP : `internal/agent/mcp_policy.go` n'appelle pas
`Authorizer.Authorize` lui-même, et n'en a pas besoin.

En lecture, l'identifiant de ressource est résolu exclusivement à partir
de `req.Conversation.Scope`/`ScopeID` (déjà déterminés par
`internal/identity.Resolver` avant que le LLM soit appelé), via
`resource.Resolve`. Il est injecté à chaque appel en écrasant ce que le modèle
aurait pu fournir sous ce nom : aucune portée alternative n'est atteignable.

En écriture, aucun appel MCP n'a lieu dans le tour : l'outil enregistre
une `delegation.ProposedAction` portant la portée résolue et la permission
requise, construite à partir du `permission_domain` déclaré pour le serveur.
Elle devient un plan persisté. C'est `internal/action.Engine` qui exécute
après confirmation, et qui applique alors les deux contrôles décrits
ci-dessus : revérification de permission (étape 5) et résolution fraîche de la
ressource (étape 6).

Ce mécanisme est agnostique du domaine. Aucun service n'est connu du code :
ressource, domaine de permission et classification lecture/écriture viennent
de `mcp_servers.<nom>`. Conséquence pour la sécurité, un serveur mal déclaré
est un risque de configuration, pas de code. La validation refuse donc
`confirm_writes` sans `permission_domain`, cas où une écriture s'exécuterait
sans contrôle d'autorisation.

Annotation `readOnlyHint` : elle est prise en compte, mais jamais comme
une autorité. Un serveur l'affirme sur lui-même et rien ne la vérifie. Elle
est donc écoutée seulement dans le sens qui restreint : un outil annoté comme
écrivant exige une confirmation même si son nom suggère une lecture. La
proposition inverse, dispenser un outil de confirmation parce qu'il se déclare
en lecture seule, exige `trust_read_only_hint: true`, à réserver aux serveurs
dont l'opérateur maîtrise le code. Par défaut, un serveur compromis qui
annoncerait une suppression comme lecture ne gagne rien
(`TestReadOnlyHint_ReadOnlyClaimIgnoredByDefault`).

Aucune décision de portée, de permission ou de ressource ne dépend d'une
valeur fournie par le modèle : vérifié par revue exhaustive des accès à
`internal/resource`, tous alimentés par le `model.Scope`/`model.ScopeID` d'une
identité résolue ou d'un plan persisté, jamais par le `map[string]any` d'un
tool-call.

### A.2. Arguments MCP forgés

**Conforme, vérifié sans régression.**

- Lectures : `wrapDirectTool` (`internal/agent/mcp_policy.go`) écrase
  systématiquement le paramètre de ressource avec la valeur résolue par
  l'application, quelle que soit celle envoyée par le modèle sous ce nom.
- Écritures : `wrapWriteTool` retire l'identifiant des arguments au
  lieu de l'y figer. Un identifiant forgé par le modèle ne survit donc pas
  jusqu'à l'action persistée (tests `..._ForgedCalendarIDNeverReachesProposedAction`,
  `..._ForgedListIDIgnored`, `TestUnknownDomain_WriteBecomesProposedAction`).
- Exécution après confirmation : `internal/action/engine.go`
  (`executeAction`, étape 6) réinjecte l'identifiant via
  `resource.InjectResolved`, à partir de la portée du plan persisté, juste
  avant l'appel MCP réel. L'action écrit donc toujours dans la ressource
  courante de sa portée, jamais dans celle qu'un modèle aurait suggérée ni
  dans celle qu'une configuration antérieure désignait.

Le scénario complet est vérifié de bout en bout par
`internal/e2e.TestPrivate_AgendaWriteConfirmedExecutesWithResolvedResource` :
le modèle y tente explicitement d'imposer `calendar_id: "forged-by-model"`, et
le serveur MCP reçoit malgré tout le calendrier de la portée.

Il n'existe donc aucun chemin où un argument MCP forgé contenant un
identifiant de ressource externe atteindrait un appel MCP. Point de vigilance
pour l'avenir : un serveur porteur d'une ressource doit la déclarer sous
`mcp_servers.<nom>.resource`, faute de quoi ses arguments passeraient
inchangés. C'est désormais une erreur de configuration possible, plus un
oubli de code.

### A.3. Injection de prompt

**Conforme.** `internal/agent/prompt.go` :

- `InvariantRules` est une constante Go, jamais interprétée comme un
  template (pas de `{{ }}`, pas de moteur de substitution) : c'est un texte
  statique concaténé tel quel.
- `BuildContextBlock` n'injecte que les 4 variables autorisées par le plan de conception
  §7.3 (agent, organisation, portée, type de canal), toutes dérivées de
  `model.ExecutionIdentity` déjà résolue par l'application, jamais de
  contenu utilisateur, jamais de résultat d'outil.
- Aucun point du pipeline ne permet à un contenu de message utilisateur de
  modifier la configuration ou les permissions : le texte du message
  n'atteint le LLM que comme message `user` dans la liste `messages`
  (`buildChatMessages`), jamais interpolé dans le system prompt ou le bloc
  de contexte.
- `InvariantRules` (règle explicite, alinéa final) prime explicitement sur
  toute instruction contraire provenant d'un message utilisateur ou d'un
  contenu récupéré via un outil. Mesure de mitigation, pas une garantie
  absolue : un LLM reste, par nature, influençable par son contexte. C'est
  pourquoi les décisions réelles (§2) ne dépendent jamais de cette
  discipline de prompt seule.

### A.4. Chemins de fichiers de prompts

**Limitation connue, risque évalué comme faible, aucune restriction
ajoutée.** `internal/config/resolve.go` (`loadSystemPrompts`) résout
`system_prompt.file` via `resolvePath(baseDir, p)`, un simple
`filepath.Join` sans neutralisation de `../`. Un chemin pourrait donc
s'échapper du répertoire du fichier de configuration. Risque jugé faible et
assumé : le fichier de configuration YAML est administré (déployé par
l'opérateur du service, jamais par un attaquant distant. Aucune valeur
d'entrée réseau n'atteint ce champ), et une restriction stricte casserait
des usages légitimes (prompts partagés en dehors de l'arborescence de
configuration). Ajouter une neutralisation ici serait une abstraction
spéculative sans menace réelle identifiée (AGENTS.md : "pas d'abstractions
spéculatives") ; documenté ici plutôt que corrigé.

### A.5. Permissions des fichiers SQLite

**Corrigé.** `internal/persistence/db.go` (`Open`) :

- Le répertoire parent est désormais créé avec `0o700` (au lieu de `0o755`).
- Le fichier de base est pré-créé explicitement avec `0o600` via
  `os.OpenFile`/`os.Chmod` avant l'ouverture par le driver SQLite, qui sinon
  applique le umask du processus (potentiellement `0644`, lisible par
  d'autres utilisateurs du système).
- Les fichiers annexes `-wal`/`-shm` (mode `journal_mode=WAL`) sont
  également restreints à `0o600` au meilleur effort après migration.
- Le même durcissement a été étendu à `internal/registry/memory.go`
  (répertoires du store mémoire Amoxtli et de l'index bleve, `0o755` →
  `0o700`) : ces répertoires contiennent également des données personnelles
  (mémoires retenues). Le fichier SQLite du store Amoxtli lui-même est ouvert
  par `amoxtligorm.NewSQLiteStore`, hors du contrôle direct d'`automata` :
  limitation connue, ses permissions dépendent de l'umask du processus ;
  seul le répertoire parent est garanti restreint.
- Test ajouté : `internal/persistence/db_test.go`,
  `TestOpenRestrictsFilePermissions`.

### A.6. Secrets

**Conforme.** Grep exhaustif de tous les usages de `slog`/`log` du dépôt
(`internal/mcp/manager.go`, `internal/ingress/pipeline.go`,
`internal/action/engine.go`, `internal/scheduler/scheduler.go`,
`internal/registry/registry.go`, `cmd/automata/main.go`) : tous les champs
journalisés sont des identifiants (`org_id`, `principal_id`,
`conversation_id`, `provider`, `server`, `session`, `tool`, `schedule_id`,
`scheduled_run_id`, `status`, `duration`, `result_bytes`, `truncated`) ou des
erreurs Go (`error`, err), jamais un secret, un jeton MCP, un en-tête
d'autorisation, ni le contenu d'un message ou d'un résultat d'outil.
`internal/mcp/manager.go` (`wrapTool`) documente explicitement ce choix en
commentaire. `internal/config` ne journalise jamais rien : `expand.go`
(résolution de `${VAR}`) et `load.go` ne contiennent aucun appel
`slog`/`log`. Les erreurs persistées par `internal/action.Engine.failAction`
sont un code court (`execution_failed`, `permission_denied`, …), jamais le
texte complet de l'erreur. Seul `actionOutcome.message` (texte affiché à
l'utilisateur final dans le rapport de plan, jamais journalisé) porte le
message complet.

### A.7. Limites de taille

**Limitation connue, documentée, non corrigée dans cette phase.** Limites
déjà en place et cohérentes : `audio.Config.MaxSize` (lecture en flux borné,
`io.LimitReader`), `attachments.max_size` / `max_count` / `max_history` /
`max_reply` (pièces jointes reçues, rejouées et renvoyées, également en
lecture bornée), `mcp.Limits.MaxToolResultBytes` (troncature des résultats
d'outil), `agents.*.limits.max_tool_context_bytes` (budget cumulé des
résultats d'outils), `agents.*.limits.max_actions_per_turn` (lot d'actions à
confirmer), `internal/conversation.defaultHistoryLimit` (20 messages
rechargés). Aucune limite n'existe en revanche sur la taille d'un
message texte brut avant persistance et envoi au LLM
(`courier.GetMessageMainContent` dans `internal/conversation/handler.go`) :
un message extrêmement long serait persisté intégralement et transmis tel
quel au LLM. Risque jugé réel mais modéré (dénis de service ciblé
essentiellement contre le portefeuille de tokens de l'opérateur, pas contre
la confidentialité ou l'intégrité des données d'autres principaux ; les
fournisseurs de messagerie et les clients LLM appliquent déjà leurs propres
plafonds) : à corriger dans une phase ultérieure d'exploitation plutôt que
par un ajout non spécifié par le plan de conception dans cette revue.

### A.8. Timeouts réseau

**Corrigé.** État avant correction : `audio.ExtractText` (borné par
`audio.Config.Timeout`) et les appels d'outils MCP (bornés par
`mcp.Limits.ToolTimeout`) étaient déjà bornés ; les exécutions planifiées par
le scheduler étaient bornées par `ScheduleConcurrency.Timeout`
(`scheduler.go`, `execCtx`). En revanche, l'appel réseau au LLM
(`client.ChatCompletion`/`ChatCompletionStream`, appelé depuis
`internal/agent/toolloop.go` et `internal/agent/agent.go`) ne recevait, pour
tout message conversationnel entrant, que le `ctx` du processus
(`signal.NotifyContext` dans `cmd/automata/main.go`, sans échéance).
`internal/agent/agent.go` documentait explicitement ce trou : "aucun
timeout n'est ajouté ici […] l'appelant est responsable d'attacher un
`context.WithTimeout` si nécessaire". Comme `ingress.Pipeline.Run` traite
les messages strictement séquentiellement (pas de goroutine par
message), un fournisseur LLM ou MCP qui ne répond jamais aurait bloqué
indéfiniment le traitement de tous les messages suivants du même
fournisseur. Un déni de service déclenchable par un tiers (fournisseur LLM
lent, serveur MCP compromis qui ne ferme jamais la connexion).

Correction apportée dans `internal/ingress/pipeline.go`
(`Pipeline.processMessage`) : le `ctx` transmis à `Handler.Handle` est
désormais borné par `handleTimeout` (5 minutes, volontairement large devant
les timeouts plus fins déjà en place pour ne jamais couper un tour légitime
enchaînant plusieurs appels d'outils) ; l'envoi de la réponse au fournisseur
courier (`provider.Send`) est borné séparément par `sendTimeout` (30
secondes). Test ajouté :
`internal/ingress/pipeline_test.go`,
`TestPipeline_HandleContextIsBounded`.

### A.9. Logs

**Conforme.** Voir A.6 : aucun log observé ne contient le contenu intégral
d'un message utilisateur, d'une transcription, ou d'arguments MCP sensibles.
Conforme à plan de conception, §14.2.

### A.10. Origines et groupes inconnus

**Conforme, aucune régression.** Les tests existants
(`internal/identity/resolver_test.go`, `internal/ingress/pipeline_test.go`,
notamment `TestPipeline_UnknownOriginIgnored`) couvrent le rejet silencieux
d'une origine inconnue et d'un groupe non déclaré ou dont l'expéditeur n'est
pas membre. Exécutés sans modification dans le cadre de cette revue : tous
verts (`go test ./...`, `go test -race ./...`).

## 4. Limitations connues assumées

- Pas de verrouillage distribué. Deux instances d'Automata sur la même base
  SQLite ne sont pas supportées. `sqlDB.SetMaxOpenConns(1)` sérialise les
  écritures dans un processus, pas entre processus, et un verrou sur le
  répertoire de données refuse la seconde instance au démarrage plutôt que
  de la laisser s'enliser.
- Chiffrement au repos partiel : `storage.encryption_key` scelle les
  contenus personnels, messages, résumés de conversation, rappels, pièces
  jointes et leurs légendes côté base applicative ; contenu des documents
  et des images côté mémoire Amoxtli, avec la même clé ; objectif et
  journal de bord des missions. Restent en clair,
  dans la base applicative, les propositions d'action et les **tâches
  planifiées** : leurs charges utiles ne passent pas par le chiffrement
  des contenus (les missions, arrivées après, chiffrent les leurs — seul
  leur titre, affiché dans des listes, reste lisible). Restent en clair, à dessein, l'enveloppe requêtable
  (identifiants, horodatages, portées) et le dictionnaire de termes de
  l'index `memory.bleve`. Chiffrer des termes indexés casserait la
  recherche. La restriction de permissions apportée en A.5 (accès local,
  propriétaire du processus) reste la protection de ce résidu.

  Le chiffrement protège une base volée, une sauvegarde égarée, un disque
  revendu. Il ne protège pas un processus compromis ni un accès root
  pendant que le service tourne : la clé y est.

  Un membre peut en outre **confier le stockage de ses rappels à un
  plugin, un agenda CalDAV typiquement (voir
  [plugins-caldav.md](plugins-caldav.md)). Leur texte quitte alors la base
  chiffrée et vit chez ce fournisseur, en clair, sous ses conditions et
  ses sauvegardes. C'est un choix explicite de la personne, pris dans son
  profil devant un avertissement qui le dit ; l'exploitant ne peut pas le
  prendre à sa place, et il ne s'applique à personne d'autre.
- Pièces jointes conservées dans la base : la table
  `message_attachments` stocke les octets des images et documents reçus,
  afin de pouvoir les rejouer dans l'historique remis au modèle. Ils sont
  chiffrés comme les autres contenus quand `storage.encryption_key` est
  renseignée, en clair sinon. C'est une décision explicite d'exploitation,
  à connaître pour trois raisons :
  - ce sont des données personnelles, souvent plus sensibles qu'un
    message texte (photo d'un lieu, d'un document, d'une personne) ; la
    sauvegarde de `/data/app.sqlite` les emporte avec elle ;
  - la base grossit bien plus vite qu'avec du texte seul (voir
    `docs/operations.md` §1) ;
  - aucune purge automatique n'est implémentée : rien ne supprime les
    pièces jointes anciennes.

  Les audios font exception et ne sont jamais stockés : notes vocales comme
  fichiers audio sont transcrits sans conservation (plan de conception, §3.4). Pour ne
  rien conserver du tout, mettre `attachments.max_history` à `0`. Les images
  restent alors visibles du modèle pour le tour courant, sans être relues
  ensuite. Ou `attachments.enabled` à `false`.
- Contenu des pièces jointes non inspecté : une image ou un document est
  transmis au fournisseur de modèle sans analyse de son contenu (ni
  antivirus, ni détection de charge utile). Le filtre `accepted_types` porte
  sur le type MIME déclaré par la plateforme, jamais sur les octets
  réellement reçus : un fichier annoncé `image/png` qui n'en est pas un
  atteindra le fournisseur, qui le rejettera. Acceptable ici parce que les
  origines sont déclarées une à une (§2), un inconnu ne peut rien envoyer,
  mais à réévaluer si les origines devenaient ouvertes.
- Chemin de fichier de prompt non neutralisé (A.4) : accepté, la
  configuration étant administrée, pas fournie par un attaquant distant.
- Le catalogue du plugin `subagents` est **du code exécuté avec les droits
  d'Automata**, au même titre qu'un binaire déposé dans le répertoire des
  plugins (voir [plugins-subagents.md](plugins-subagents.md)). Une entrée
  peut télécharger un binaire et le lancer, hors de tout bac à sable —
  contrairement à l'atelier `workspace`, qui passe par LeaSH. Le fichier
  doit rester sous le contrôle exclusif de l'exploitant. Trois garde-fous,
  qui ne remplacent pas cette exigence : la source d'une URL ou d'une
  commande n'est jamais un membre ni le modèle ; un téléchargement sans
  somme de contrôle déclarée est refusé au chargement du catalogue, et une
  somme qui ne correspond pas annule l'installation ; les identifiants d'un
  membre ne peuvent pas figurer dans une installation, posée une fois pour
  tous. Un serveur MCP ainsi lancé sonde ou appelle **depuis le serveur** :
  c'est à sa propre configuration de borner ce qu'il peut atteindre, et
  c'est ce que fait la politique de `netprobe`.
- Aucune limite de taille sur un message texte brut (A.7). Accepté, risque
  modéré. Les plateformes bornent déjà ce qu'elles transportent.
- Nouveau serveur MCP porteur d'une ressource résolue (A.2) : il doit
  être déclaré dans `internal/resource` (constantes et `InjectResolved`),
  faute de quoi ses arguments atteindraient l'appel réel sans réinjection de
  la ressource de la portée.
- Fichier SQLite du store mémoire Amoxtli : permissions dépendantes de
  l'umask du processus, hors du contrôle direct d'`automata` (bibliothèque
  externe) ; seul le répertoire parent est garanti restreint (A.5).
- La discipline de prompt (`InvariantRules`) est une mitigation, pas une
  garantie (A.3). Un modèle reste influençable par son contexte. C'est
  pourquoi aucune décision de sécurité réelle (§2) ne repose sur elle seule,
  et c'est le principe de tout ce document.
