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
| [integration-inventory.md](integration-inventory.md) | API réelles de go-courier, genai et Amoxtli, et leurs manques |

Pour une première installation, lisez `installation.md` en entier, puis la
section de `configuration.md` correspondant à ce que vous voulez activer.
`agents.md` n'est utile qu'au moment d'ajouter un spécialiste.

## Ce qu'Automata fait

- Conversations privées et de groupe. Dans un groupe, un message est ignoré
  sans le moindre appel au modèle tant que l'assistant n'est pas mentionné.
- Messages texte, notes vocales (transcrites, jamais conservées), images et
  documents.
- Un agent généraliste qui délègue à des spécialistes. Chacun a son prompt,
  ses serveurs MCP, ses permissions et ses limites.
- Une mémoire persistante cloisonnée par portée, consultable, inscriptible et
  supprimable — y compris **par la personne concernée**, qui voit dans son
  profil ce qu'Automata retient d'elle, mot pour mot, et peut le corriger ou
  l'effacer. Seuls ses souvenirs personnels y figurent : ceux d'un groupe
  appartiennent au groupe.
- Des plans d'actions confirmés en toutes lettres avant toute écriture
  externe.
- Des tâches planifiées qui s'exécutent sous une identité de service, en
  lecture seule ou en proposant des actions à confirmer.
- Un accueil pour les nouveaux arrivants : quatre questions dans le fil de la
  conversation, dont les réponses deviennent des souvenirs personnels
  ordinaires, et une page « Découvrir » dans le profil. La visite se quitte
  d'un mot (« passe ») et ne se repropose jamais — et une vraie question
  posée à la place d'une réponse y met fin d'elle-même, l'assistant reprenant
  la main sur le message.
- Un casier personnel : ce que l'assistant garde pour vous survit des mois,
  scellé au repos, là où les fichiers de travail expirent en un jour
  (voir [plugins-workspace.md](plugins-workspace.md)).
- Des alertes d'exploitation portées à l'exploitant dans sa propre
  conversation (voir [operations.md](operations.md), §4.1).
- Une introspection hebdomadaire : Automata relit les frictions (actions
  jamais confirmées, rappels en échec) et les habitudes observées, et
  propose à chaque membre au plus une amélioration par semaine — sur sa
  page de profil, poussée en conversation seulement quand le gain est net.
  « Ne plus rien me proposer » coupe tout, définitivement. L'exploitant
  reçoit chaque mois une synthèse anonyme des frictions par type, sans nom
  ni contenu.

## Ce qu'il ne fait pas

- Pas de haute disponibilité. Une seule instance à la fois, sans verrouillage
  distribué.
- Pas de chiffrement de la machine. Les contenus personnels (messages,
  résumés, rappels, pièces jointes, souvenirs) sont chiffrés au repos dès
  que `storage.encryption_key` est renseignée, ce qui protège une base
  volée ou une sauvegarde égarée — pas un serveur compromis, qui détient
  la clé. L'index de recherche de la mémoire reste en clair par nécessité.
- Un seul transport de messagerie livré, WhatsApp via go-courier.
- Un seul transport MCP, HTTP. Pas de serveur MCP lancé en sous-processus.
