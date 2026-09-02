# Exploitation

Ce document couvre l'exploitation d'une instance : diagnostiquer une
panne, sauvegarder et restaurer les données, et redéployer une instance,
sans jamais avoir besoin de lire le contenu des conversations privées
(AGENTS.md, "ne pas journaliser les contenus privés". Le même principe
gouverne l'exploitation).

## 1. Fichiers à sauvegarder

Une instance Automata persiste son état dans quatre emplacements distincts,
tous sous le répertoire de données de l'instance (`/data` dans les exemples
ci-dessous, à adapter à `storage.application.path`, `memory.store.path` et au
`session_path` de chaque compte WhatsApp, saisi dans l'administration) :

| Emplacement                          | Contenu                                                              | Config source                     |
|---------------------------------------|------------------------------------------------------------------------|------------------------------------|
| `/data/app.sqlite` (+ `-wal`, `-shm` si présents) | Base applicative : conversations, messages, pièces jointes, plans d'actions, exécutions planifiées, tentatives de livraison, audit | `storage.application.path`         |
| `/data/amoxtli.sqlite`                | Métadonnées de la mémoire persistante (Amoxtli)                       | `memory.store.path`                |
| `/data/memory.bleve/`                 | Index de recherche plein texte de la mémoire                          | `memory.indexes[].path`            |
| `/data/courier/`                      | Session WhatsApp (identifiants d'appareil liés, état Go Courier)      | `session_path` du compte (administration, "Canaux et plateformes") |

La base applicative fonctionne en mode WAL (`storage.application.pragmas.journal_mode`) :
les fichiers `-wal` et `-shm` associés à `app.sqlite`, lorsqu'ils existent,
font partie intégrante de l'état non encore consolidé dans le fichier
principal et doivent être copiés avec lui, jamais séparément.

### Croissance liée aux pièces jointes

Lorsque `attachments.enabled` vaut `true`, la table `message_attachments`
conserve les octets bruts des images et documents reçus, afin de pouvoir
les rejouer dans l'historique remis au modèle. `app.sqlite` grossit alors bien
plus vite qu'avec du texte seul : avec `max_size: 8MiB` et `max_count: 4`, un
seul message peut ajouter jusqu'à 32 Mio.

Trois conséquences pratiques :

- **Aucune purge automatique n'est implémentée.** Rien ne supprime les pièces
  jointes anciennes ; la base croît de façon monotone tant qu'un opérateur
  n'intervient pas.
- Ces octets sont des données personnelles et partent dans chaque
  sauvegarde (voir `docs/security-model.md` §4).
- Les audios ne sont jamais concernés : notes vocales et fichiers audio sont
  transcrits sans conservation.

Surveiller la taille et, au besoin, purger les pièces jointes antérieures à
une date, service arrêté :

```bash
sqlite3 /data/app.sqlite "SELECT COUNT(*), SUM(LENGTH(data))/1024/1024 || ' Mio' FROM message_attachments;"

# Purge des pièces jointes de plus de 90 jours. Les messages, eux, sont
# conservés : seul leur média disparaît de l'historique rejoué.
sqlite3 /data/app.sqlite "DELETE FROM message_attachments WHERE created_at < date('now', '-90 days');"
sqlite3 /data/app.sqlite "VACUUM;"
```

Pour ne rien conserver du tout, mettre `attachments.max_history` à `0` : les
pièces jointes restent visibles du modèle pour le tour courant, mais ne sont
plus relues ensuite.

## 2. Procédure de sauvegarde

Méthode recommandée : arrêt propre avant copie.

Automata s'arrête proprement sur `SIGINT`/`SIGTERM` (`context.Context` annulé
dans `cmd/automata/main.go`, propagé par `internal/registry.Run` à tous les
composants : pipelines ingress, scheduler, serveur d'observabilité). Une
sauvegarde prise pendant que le processus tourne risquerait de copier
`app.sqlite` pendant une transaction en cours ou de laisser les fichiers WAL
et le fichier principal dans des états incohérents entre eux malgré un
`cp` atomique par fichier (aucune garantie de cohérence *inter-fichiers*
sans coordination applicative). L'arrêt propre reste donc la méthode par
défaut retenue ici : la plus simple à documenter et à exécuter sans risque,
même si SQLite propose par ailleurs une API de sauvegarde à chaud
(`VACUUM INTO`, ou l'API C `sqlite3_backup`) qui pourrait éviter l'arrêt du
service. Elle n'est pas retenue par défaut pour ne pas ajouter une dépendance
opérationnelle supplémentaire (outillage `sqlite3` CLI, ou code applicatif
dédié) non demandée explicitement par plan de conception, §20.

Séquence :

1. Envoyer `SIGTERM` au processus (ou `Ctrl+C` en premier plan) et attendre
   sa sortie. Le processus journalise `"automata stopping"` puis se termine
   une fois tous les composants arrêtés (`wg.Wait()` dans
   `internal/registry.Run`).
2. Copier l'intégralité des quatre emplacements du tableau ci-dessus vers la
   destination de sauvegarde (ex. `tar`, `rsync`, snapshot du volume).
3. Redémarrer le processus (voir §5 "Mise à jour et redémarrage").

Une sauvegarde régulière (cron, ou tâche planifiée hors du processus
Automata lui-même) doit exécuter ces trois étapes ; la fenêtre
d'indisponibilité correspond à la durée de la copie (généralement
quelques secondes à quelques minutes selon la taille de l'index Bleve).

## 3. Procédure de restauration

1. Arrêter le processus s'il tourne (même procédure que pour la
   sauvegarde).
2. Remplacer intégralement les quatre emplacements par leur contenu
   sauvegardé (supprimer l'état courant avant de restaurer, pour ne pas
   laisser cohabiter un `-wal` restauré avec un `app.sqlite` courant, par
   exemple).
3. Vérifier que les permissions et le propriétaire des fichiers restaurés
   correspondent à ceux attendus par le processus (voir
   `docs/security-model.md` pour les permissions restrictives appliquées à
   la base SQLite).
4. Redémarrer le processus. Les migrations s'appliquent automatiquement à
   l'ouverture (voir §4) ; aucune étape manuelle supplémentaire n'est
   nécessaire.
5. Valider la restauration avec `automata admin inspect` (voir §6) et les
   endpoints de santé (voir §4) avant de considérer l'instance
   opérationnelle.

## 4. Endpoints de santé et de métriques

Un serveur HTTP local optionnel peut être activé dans la configuration :

```yaml
observability:
  enabled: true
  addr: "127.0.0.1:9090"
```

Section absente, ou `enabled: false` (par défaut) : aucun serveur HTTP
n'est démarré, comportement historique inchangé. Lorsqu'il est activé, il
expose trois routes, toutes en lecture seule et sans authentification.
l'adresse doit donc rester locale (`127.0.0.1`) ou protégée par un
pare-feu/reverse proxy si elle doit être exposée à un superviseur externe :

- `GET /healthz/live` : 200 dès que le processus tourne, indépendamment
  de tout état interne (liveness). Un superviseur de processus (systemd,
  Docker, Kubernetes) l'utilise pour décider s'il faut redémarrer le
  processus.
- `GET /healthz/ready` : 200 une fois que la persistance est ouverte et
  que les pipelines ingress et le scheduler ont démarré ; 503 avant (au
  démarrage) ou si l'instance ne doit pas encore recevoir de trafic
  applicatif (readiness). Un load-balancer ou une sonde d'orchestrateur
  l'utilise pour décider d'aiguiller du trafic vers cette instance.
- `GET /metrics` : export JSON agrégé des compteurs et latences
  décrits par plan de conception, §14.3 : messages reçus, messages ignorés sans
  mention, origines refusées, messages dupliqués, latence de transcription
  et de réponse (compte/somme/min/max en millisecondes), délégations par
  agent, appels MCP réussis/en erreur par (serveur, outil), actions
  proposées/confirmées, recherches mémoire, occurrences cron par schedule,
  erreurs de livraison, résultats d'outil tronqués. Aucun contenu de
  message, de transcription ni d'argument d'outil n'y figure jamais :
  uniquement des compteurs agrégés et des identifiants déjà présents dans
  la configuration (`agent_id`, `schedule_id`, `server`/`tool` MCP).

Interrogation typique :

```bash
curl -s http://127.0.0.1:9090/healthz/live
curl -s http://127.0.0.1:9090/healthz/ready
curl -s http://127.0.0.1:9090/metrics | jq .
```

Format de l'export retenu : JSON simple (pas le format texte Prometheus).
plan de conception, §14.3 ne prescrit aucun format particulier ; un export Prometheus
aurait ajouté du travail de mise en forme (types de métriques, labels,
échappement) sans bénéfice pour l'instant. Un
`curl | jq` suffit à diagnostiquer une panne. Si une intégration Prometheus
devient nécessaire plus tard, un exporteur séparé peut consommer `/metrics`
sans modifier `internal/observability`.

## 4.1 Alertes d'exploitation

`/metrics` et `/healthz` disent l'état du service, mais il faut aller les
regarder. Une session WhatsApp perdue un vendredi soir ne se remarque
autrement qu'au premier message resté sans réponse, le lundi.

L'instance peut donc prévenir un membre, l'exploitant, dans sa conversation
privée avec Automata. Le destinataire se désigne dans l'administration,
écran Alertes ; seul un membre déjà rattaché à une conversation peut être
choisi, puisque c'est par là que l'alerte arrive. Aucun destinataire désigné
n'empêche rien : les alertes restent enregistrées et consultables sur cet
écran, et repartiront le jour où quelqu'un est désigné.

Une veille inspecte l'instance toutes les cinq minutes et alerte sur :

- un compte de messagerie en échec depuis plus de dix minutes ;
- un compte muet : il se déclare en marche, mais ne répond plus quand on
  l'interroge (voir ci-dessous) ;
- un plugin arrêté depuis plus de dix minutes, ou qui n'a jamais démarré.

Pourquoi une sonde en plus de l'état. L'état d'un compte ne devient "en
échec" que si son pipeline se termine. Un fournisseur bloqué sur un
verrou ou sur une socket morte reste indéfiniment "en marche" alors qu'il
ne reçoit plus rien : le pire des cas, puisque rien ne le signale. C'est ce
qui est arrivé le 2026-08-30, où le compte Rocket.Chat est resté muet toute
une nuit. La veille interroge donc chaque compte réputé en marche
(`Self`, avec cinq secondes de patience) ; une absence de réponse déclenche
une alerte distincte, `platform_mute`, dont le remède est un redémarrage du
service.

Le délai de grâce écarte le transitoire : un compte qui redémarre, un plugin
relancé, un appairage en cours n'ont pas à réveiller qui que ce soit. Une
alerte identique ne repart pas avant une heure. Une conversation inondée ne
se lit plus, ce qui revient à n'alerter personne.

Limite à connaître. L'alerte passe par la messagerie : quand la panne EST
la messagerie, elle ne peut pas partir. Elle est alors journalisée en ERROR,
conservée "non remise", et rejouée dès que le canal revient. L'écran des
alertes distingue les deux cas ; une alerte non remise signale que le problème
a bien été vu, mais que personne n'a été prévenu. Un repli par courriel
couvrirait ce cas. Il n'est pas en place.

Le journal des alertes est conservé un mois, puis purgé.

## 4.2 Lire les journaux

Les journaux d'Automata sont structurés en JSON sur la sortie d'erreur, au
niveau INFO. Deux composants écrivent à côté, dans leur propre format :

- whatsmeow (la bibliothèque WhatsApp) écrit sur la sortie standard, à un
  niveau aligné sur celui de l'instance. En DEBUG il imprime chaque trame du
  protocole, dont une paire de maintien toutes les vingt-cinq secondes : de
  quoi ensevelir tout le reste. Ne l'activez que pour examiner le protocole
  lui-même.
- Les plugins journalisent par leur propre canal, également aligné.

Pour isoler ce qui vient d'Automata dans une sortie mêlée :

```sh
dokku logs automata --num 20000 | grep -av "Client/" | grep -aE 'level=(WARN|ERROR)|"level":"(WARN|ERROR)"'
```

## 5. Mise à jour et redémarrage

1. Arrêter proprement le processus (`SIGTERM`, attendre la sortie, voir
   §2). Ne jamais tuer le processus (`SIGKILL`) en fonctionnement normal :
   cela laisserait potentiellement un plan d'actions bloqué en
   `executing` (récupéré automatiquement au redémarrage suivant par
   `internal/action.Engine.RecoverInterrupted`, mais avec les actions
   concernées marquées en échec plutôt que rejouées. Voir le commentaire
   de cette fonction).
2. Remplacer le binaire `automata` par la nouvelle version.
3. Redémarrer le processus avec la même commande
   (`automata -config <chemin>`). Les migrations de schéma en attente
   s'appliquent automatiquement à l'ouverture de la base
   (`internal/persistence.Open`) : il n'existe pas de commande
   `automata migrate` séparée. Ce choix est délibéré, pas un oubli : les
   migrations sont déjà idempotentes (rejouables sans effet si déjà
   appliquées) et s'exécutent avant tout traitement de message ou tick de
   scheduler, donc une commande dédiée n'apporterait qu'une redondance
   avec le comportement automatique existant, sans réduire aucun risque
   opérationnel réel (le seul scénario où "migrer sans démarrer le
   service complet" aurait de la valeur, vérifier qu'une migration
   s'applique sans risque avant un déploiement, est déjà couvert par
   `automata config validate`, qui charge la configuration sans ouvrir la
   base ni démarrer de service).
4. Valider avec `GET /healthz/ready` (si activé) et `automata admin
   inspect -kind runs` que le service a repris normalement.

## 6. Commandes disponibles

| Commande | Rôle |
|---|---|
| `automata -config <fichier>` | Démarre le service |
| `automata config init [-output f] [-env-output f]` | Génère une configuration en répondant à des questions |
| `automata config validate -config <fichier>` | Valide sans rien démarrer |
| `automata healthcheck [-addr h:p] [-timeout d]` | Sonde de santé, code de sortie 0 ou 1 |
| `automata memory reindex -config <fichier>` | Reconstruit l'index de la mémoire depuis le store |
| `automata admin inspect -config <fichier> [-kind plans\|runs]` | Inspecte les plans d'actions ou les exécutions planifiées |

Toutes utilisent le tiret simple, convention Go, pas le double tiret.

## 6.1 Commandes d'administration en détail

Ces commandes existent déjà (Phases 2, 10, 18) ; ce paragraphe n'en est
qu'un récapitulatif opérationnel, pas une redocumentation complète (voir
`cmd/automata/main.go` pour le détail de chaque sous-commande).

- `automata config validate -config <chemin>` : charge et valide
  intégralement une configuration YAML sans démarrer de service. À exécuter
  avant tout déploiement d'une configuration modifiée.
- `automata memory reindex -config <chemin>` : réindexe intégralement
  la mémoire persistante à partir du store, sans démarrer le service
  complet. Utile après une modification de `memory.indexes` ou une
  restauration de `/data/amoxtli.sqlite` sans son index Bleve associé.
- `automata admin inspect -config <chemin> -kind plans|runs` :
  inspection en lecture seule (aucune mutation) de l'état récent des plans
  d'actions (`plans`) ou des exécutions planifiées (`runs`). Première
  commande à exécuter pour diagnostiquer un plan bloqué, une confirmation
  qui semble ne jamais aboutir, ou un schedule qui ne se déclenche pas.

## 6.2 Liens de profil et aperçus de messagerie

Un lien de profil ne se consomme pas à la lecture : la page qu'il ouvre
présente un bouton "Ouvrir mon profil", et c'est ce POST qui grille
le lien et ouvre la session.

C'est délibéré. Les messageries préchargent les adresses qu'on y colle
pour en afficher un aperçu. Rocket.Chat, Slack, WhatsApp ouvrent le lien
avant tout humain. Un lien à usage unique consommé par un GET arrivait
donc déjà mort chez son destinataire, qui n'y lisait que "Ce lien a déjà
servi". Aucun robot d'aperçu n'émet de POST.

## 6.3 Détacher un canal

Fiche de l'organisation → onglet Canaux → bouton "Détacher" sur la ligne.
Automata cesse d'y répondre dès le message suivant, sans redémarrage. La
conversation et son historique restent en base. Détacher n'est pas effacer.
Pour effacer, c'est le membre (RGPD) ou l'organisation entière.

Seuls les canaux rattachés en ligne, par jeton, ont ce bouton. Un canal
déclaré dans le fichier de configuration est marqué "fichier" et se retire
en éditant le fichier.

## 6.4 Supprimer une organisation

Fiche de l'organisation → onglet Personnalisation → bloc rouge en bas.
Le nom de l'organisation se retape pour confirmer : deux organisations
peuvent être homonymes, et l'effacement est sans retour.

Ce que la suppression emporte :

- les membres de l'organisation, avec leurs liens de profil et leurs
  jetons ;
- ses canaux rattachés. Les conversations concernées redeviennent
  inconnues de l'instance ;
- ses conversations, avec messages, résumés, pièces jointes et plans
  d'actions ;
- ses rappels, ses tâches planifiées et ses événements d'audit ;
- ses réglages, ses activations de plugins et leurs secrets ;
- ses souvenirs, dans la base mémoire, personnels comme collectifs.

Ce qui reste : **les relevés de consommation et les mouvements de
portefeuille**, dissociés de la personne et de la conversation. Ce sont
des pièces comptables ; une recette ou un coût constaté ne s'efface pas
parce qu'un client s'en va.

Un membre n'appartient qu'à une organisation : sa ligne lui est propre.
Une personne membre de deux organisations avec le même compte de
messagerie garde donc l'autre profil intact. C'est le journal
(`orphan_members`) qui dit combien de personnes ont perdu là leur dernier
rattachement.

La suppression touche deux bases (applicative et mémoire) et n'est pas
transactionnelle entre elles : les souvenirs partent en premier,
délibérément. Si la suite échoue, l'organisation survit sans sa mémoire,
état réparable, alors que l'inverse laisserait des souvenirs sans
propriétaire.

## 7. Diagnostiquer une panne sans lire les conversations privées

Ordre recommandé, du plus rapide au plus détaillé :

1. `GET /healthz/live` et `/healthz/ready` (si le serveur d'observabilité
   est activé) : le processus tourne-t-il, est-il prêt ?
2. `GET /metrics` : y a-t-il un pic d'erreurs de livraison, d'appels MCP en
   erreur, d'origines refusées, de résultats tronqués ? Ces compteurs
   suffisent souvent à localiser le composant en cause (ingress, MCP,
   scheduler, livraison) sans consulter aucun contenu applicatif.
3. `automata admin inspect -kind plans` / `-kind runs` : état détaillé,
   toujours sans contenu de conversation (statuts, horodatages,
   identifiants).
4. Logs structurés du processus (`slog`, JSON sur stderr) : toujours
   filtrés des contenus privés par construction (plan de conception, §14.2, jamais de
   contenu intégral de message, de transcription, d'arguments MCP
   sensibles, de tokens ni de pièces jointes), mais avec les identifiants
   de corrélation utiles (`org_id`, `principal_id`, `conversation_id`,
   `provider`, `channel_id`, `trigger`, `agent_id`, `schedule_id`, `run_id`,
   `action_plan_id`, `action_id`).
