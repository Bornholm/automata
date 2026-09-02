# Documentation Automata

Automata est un assistant conversationnel qui reçoit des messages WhatsApp,
les confie à un agent généraliste, et laisse cet agent déléguer à des
spécialistes branchés sur des serveurs MCP. Il tient une mémoire persistante,
demande confirmation avant toute écriture sensible, et sait se déclencher tout
seul sur des expressions cron.

## Par où commencer

| Document | Ce qu'il couvre |
|---|---|
| [installation.md](installation.md) | Prérequis, compilation, première configuration, démarrage, vérification |
| [configuration.md](configuration.md) | Référence de chaque section du fichier YAML |
| [agents.md](agents.md) | Créer ses propres sous-agents, leur brancher des MCP, authentifier chaque utilisateur séparément |
| [operations.md](operations.md) | Sauvegarde, restauration, mise à jour, diagnostic, métriques |
| [deployment.md](deployment.md) | Image Docker, volumes, sonde de santé, arrêt gracieux |
| [../misc/dokku/README.md](../misc/dokku/README.md) | Déploiement sur Dokku (`make dokku-deploy`) |
| [plugins-email.md](plugins-email.md) | Plugin de boîte mail : connexion Gmail, courriels entrants |
| [plugins-caldav.md](plugins-caldav.md) | Plugin d'agenda : rappels rangés dans un agenda CalDAV |
| [plugins-workspace.md](plugins-workspace.md) | Plugin d'atelier de fichiers : retouche vidéo et image en bac à sable |
| [plugins-pages.md](plugins-pages.md) | Plugin de pages web : brouillons par membre, publication confirmée sous lien public court |
| [skills.md](skills.md) | Bibliothèque de compétences : modes opératoires chargés à la demande par les agents |
| [models.md](models.md) | Catalogue de modèles : édition depuis l'administration, modèles par organisation, organisations gratuites sans limite |
| [security-model.md](security-model.md) | Modèle de menace, frontières de confiance, limitations assumées |

Pour une première installation, lisez `installation.md` en entier, puis la
section de `configuration.md` correspondant à ce que vous voulez activer.
`agents.md` n'est utile qu'au moment d'ajouter un spécialiste.

## Ce qu'Automata fait

Il tient des conversations privées et de groupe. Dans un groupe, il n'appelle
aucun modèle tant qu'on ne l'a pas mentionné. Un vocal fait exception, parce
qu'un audio ne peut pas porter de mention. Il est transcrit, et si votre nom
pour l'assistant n'y est pas, la transcription est jetée.

Il lit du texte, des notes vocales (transcrites, jamais conservées), des
images et des documents.

Un agent généraliste reçoit tout et délègue à des spécialistes. Chaque
spécialiste a son prompt, ses serveurs MCP, ses permissions et ses limites
de dépense.

La mémoire est cloisonnée par personne, par groupe et par organisation. La
personne concernée la voit dans son profil, mot pour mot, et peut corriger
ou effacer. Elle ne voit que ses souvenirs personnels. Ceux d'un groupe
appartiennent au groupe.

Toute écriture externe passe par un plan d'actions confirmé en toutes
lettres. Les tâches planifiées tournent sous une identité de service, en
lecture seule ou en proposant des actions à confirmer.

Une affaire qui s'étale sur des semaines — une réclamation à suivre, une
réponse à attendre — devient une mission : l'agent tient un journal de
bord, se réveille aux échéances qu'il se fixe, et prépare les relances
sous forme de plans à confirmer. Le dossier se lit et s'abandonne depuis
l'onglet Dossiers du profil.

Un nouvel arrivant se voit proposer quatre questions dans le fil de la
conversation. Les réponses deviennent des souvenirs ordinaires. "Passe" y
met fin, et une vraie question posée à la place d'une réponse aussi. Une
page "Découvrir" dans le profil donne des phrases à recopier pour essayer
chaque capacité.

Le casier garde des documents pendant des mois, chiffrés au repos, là où
les fichiers de l'atelier expirent en un jour. Voir
[plugins-workspace.md](plugins-workspace.md).

L'exploitant reçoit les alertes dans sa propre conversation. Un compte de
messagerie muet, un plugin arrêté. Voir [operations.md](operations.md).

Chaque lundi, Automata relit les frictions de la semaine (actions jamais
confirmées, rappels en échec) et les habitudes qu'il a observées, et
propose à chaque membre au plus une amélioration. Elle attend sur le
profil. Il ne la pousse en conversation que si le gain est net. "Ne plus
rien me proposer" coupe tout. L'exploitant reçoit une synthèse mensuelle
des frictions par type, sans nom ni contenu.

## Ce qu'il ne fait pas

Pas de haute disponibilité. Une instance à la fois, et un verrou sur le
répertoire de données pour empêcher la seconde de démarrer.

Pas de protection contre un serveur compromis. Les contenus personnels sont
chiffrés au repos dès que `storage.encryption_key` est renseignée, ce qui
protège une base volée ou une sauvegarde égarée. Pas la machine qui détient
la clé. L'index de recherche de la mémoire reste en clair, il n'y a pas
d'autre façon de chercher dedans.

Un seul transport MCP, HTTP. Pas de serveur MCP lancé en sous-processus.
