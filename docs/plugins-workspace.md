# Plugin Workspace

Le plugin `workspace` donne à Automata un **atelier de fichiers** : un
membre envoie une vidéo ou une image par sa messagerie, demande une
opération dessus (« recadre cette vidéo », « retire le bas de l'image »),
et reçoit le fichier transformé en pièce jointe de la réponse.

Il en va de même des documents : envoyer un `.docx` et demander une
correction, faire produire un rapport en PDF, convertir un `.odt`.

Le travail se fait avec `ffmpeg` et `imagemagick` pour les médias, `pandoc`
et LibreOffice pour les documents, dans un bac à sable
[LeaSH](https://github.com/Bornholm/leash) — un service séparé, sans accès
réseau, où chaque membre a son propre répertoire.

Aucune interface : tout passe par la conversation.

## Ce qui se passe pendant un tour

1. La pièce jointe arrive. Son type figure dans `attachments.tool_types`
   (voir [configuration.md](configuration.md)) : elle est conservée mais
   **jamais** transmise au modèle, qui n'en reçoit que le nom, le type et la
   taille. Une vidéo envoyée à un fournisseur texte-seul ferait échouer la
   requête entière.
2. L'orchestrateur délègue à `delegate_to_workspace`.
3. Le sous-agent appelle `import_attachment` : l'hôte pousse les octets au
   plugin, qui les dépose dans le workspace du membre côté LeaSH.
4. Il inspecte (`ffprobe`, `identify`), puis transforme (`run_command`).
5. Quand la demande dépend de ce à quoi ressemble le média — repérer un logo
   à masquer, vérifier un rendu — il appelle `view_file` : l'hôte soumet
   l'image au modèle multimodal et lui rend la réponse en texte. Pour une
   vidéo, il extrait d'abord une trame avec `ffmpeg`.
6. Il appelle `attach_file` : l'hôte récupère le résultat et le joint à la
   réponse. Si le fichier dépasse la limite des pièces jointes, il appelle
   `share_file` et donne le lien de téléchargement (voir plus bas).

Les octets ne traversent jamais la conversation : le modèle ne voit que des
chemins, des noms et des tailles.

**Envoyer le fichier puis demander au message suivant fonctionne.**
L'agent peut importer n'importe quel fichier reçu dans la conversation, pas
seulement celui joint au message auquel il répond : l'orchestrateur lui
transmet la liste des pièces `tool_only` de l'historique récent (borné par
`attachments.max_history`), et il les désigne par leur nom. À noms égaux,
le fichier du message courant l'emporte.

Une fois importé, le fichier reste dans le workspace (`LEASH_TTL`, 24 h par
défaut) : aux tours suivants, l'agent le retrouve avec `list_files` sans
réimport.

Cette transmission ne va qu'aux délégués qui **déclarent** savoir manipuler
des fichiers (`delegation.FileCapable`) : le socle interroge une capacité,
il ne connaît aucun plugin par son nom. Le spécialiste `vision`, par
exemple, n'en reçoit rien. Et ces fichiers restent invisibles du modèle —
seuls les outils fichiers peuvent aller les chercher.

## Voir ce qu'il manipule

Le modèle qui pilote le sous-agent est texte-seul : sans aide, il travaille
en aveugle. Un agent aveugle à qui l'on demande de retirer un logo tente de
« regarder » l'image en dumpant des histogrammes zone par zone — il épuise
son budget d'appels d'outils avant d'avoir produit le moindre fichier.

`view_file` lui donne des yeux : il désigne une image de son workspace et
pose sa question ; l'hôte la soumet au client `plugins.vision_client` et lui
rend la réponse. Le prompt du modèle de vision lui demande de répondre en
coordonnées exploitables (`x, y, largeur, hauteur`, origine en haut à
gauche, avec les dimensions de référence) plutôt qu'en description d'image.

L'outil refuse tout ce qui n'est pas une image, et le dit avec la commande
à lancer : une vidéo entière coûterait cher et serait refusée par la
plupart des fournisseurs. Sans `plugins.vision_client` configuré, l'outil
n'est pas monté du tout — l'hôte ne propose que ce qu'il sait servir.

## Pourquoi les commandes ne demandent pas de confirmation

Les outils d'exécution sont déclarés `read_only`, à dessein. Une commande
lancée dans le bac à sable — réseau coupé, seul le workspace du membre
monté en écriture — n'a d'effet que sur les fichiers de ce membre. La
frontière de sécurité est le bac à sable, pas la confirmation ; exiger
« confirmer » à chaque commande `ffmpeg` rendrait l'agent inutilisable.

L'invariant du dépôt reste entier : tout outil qui écrit **hors** du bac à
sable passe par une action proposée. Rien ici n'écrit hors du bac à sable.

**Corollaire** : le réseau ne doit jamais être ouvert dans la policy de ce
bac à sable sans revenir sur cette décision. C'est aussi pourquoi la
recherche d'information sur Internet n'a pas sa place ici : elle passe par
le spécialiste `research` et son serveur MCP, et le résultat arrive au
workspace sous forme de texte dans la délégation.

**Interpréteurs** : `python3` est présent dans l'image (`ocrmypdf` en
dépend et s'exécute par son shebang) mais n'est PAS dans la liste blanche.
L'y ajouter changerait ce que cette liste signifie — elle ne dirait plus ce
que l'agent peut calculer, seulement ce qu'il peut atteindre. Décision
d'exploitation, à ne pas prendre à la légère.

## Configuration

Deux variables d'environnement, héritées de l'hôte — il n'y a rien à régler
par membre :

| Variable            | Description                                                    |
| ------------------- | -------------------------------------------------------------- |
| `LEASH_SERVER_URL`  | URL du serveur LeaSH, sur le réseau interne (jamais publique). |
| `LEASH_API_KEY`     | Clé API de l'instance Automata auprès de ce serveur.           |
| `LEASH_FETCH_API_KEY` | Clé de la policy `fetch.yaml` (téléchargement). Vide : l'outil `download_video` n'existe pas. |
| `WORKSPACE_DOWNLOAD_DOMAINS` | Liste blanche des sites téléchargeables, séparés par des virgules. Vide : YouTube, Vimeo, Dailymotion. |

### Télécharger une vidéo

L'atelier n'a **pas** le réseau, et c'est ce qui rend `run_command`
exécutable sans confirmation. Le téléchargement passe donc par une
**seconde clé API** pointant sur `policies/fetch.yaml` côté LeaSH : une
policy dont le seul binaire autorisé est `fetch-video` (un encadrement de
`yt-dlp` — ni `--exec`, ni playlist, ni post-processeur, plafonds de taille
et de durée), sur le même workspace du membre. Deux clés, deux policies,
un seul atelier :

```
LEASH_APIKEY_AUTOMATA=…        LEASH_APIKEY_AUTOMATA_POLICY=/app/policies/toolbox.yaml
LEASH_APIKEY_FETCH=…           LEASH_APIKEY_FETCH_POLICY=/app/policies/fetch.yaml
```

L'outil `download_video` n'apparaît pour le modèle que si
`LEASH_FETCH_API_KEY` est renseignée. L'URL est validée **deux fois** :
côté Automata contre `WORKSPACE_DOWNLOAD_DOMAINS` (schéma http(s),
sous-domaines acceptés, adresses IP littérales refusées — la forme
qu'aurait une tentative vers un service interne), puis par le script
lui-même. Élargir la liste blanche est un geste d'exploitation ; élargir
la policy LeaSH d'un binaire n'en est pas un.

Côté `config.yaml` :

```yaml
plugins:
  client: main
  # Client multimodal de view_file : celui du sous-agent est texte-seul.
  vision_client: vision

attachments:
  tool_types:
    - video/mp4
    - video/webm
    - video/quicktime
  max_tool_size: 16MiB
```

`max_tool_size` borne les pièces entrantes **et** les fichiers renvoyés en
pièce jointe. 16 Mio correspond à la limite d'une vidéo WhatsApp ; le
prompt du sous-agent lui demande de viser ~15 Mio. Au-delà, le fichier
repart par un lien (section suivante), et non plus par la messagerie.

Le plugin s'active ensuite par organisation, dans l'onglet Plugins de la
fiche organisation.

## Récupérer un fichier trop lourd

Une vidéo de trente minutes pèse plus de cent mégaoctets : aucune
messagerie ne l'accepte. `share_file` est la seconde voie de sortie — un
**lien de téléchargement temporaire** que l'assistant donne dans la
conversation.

- **Valable 24 h**, comme l'espace de travail lui-même : un lien plus long
  désignerait un fichier déjà effacé. Chaque accès au fichier rafraîchit ce
  délai côté LeaSH, si bien qu'un lien utilisé garde son contenu en vie.
- **Sans compte ni authentification** : le lien porte un jeton signé, non
  devinable, qui n'ouvre qu'UN chemin précis de l'espace de travail d'UN
  membre. Quiconque l'obtient télécharge le fichier — c'est ce qui le rend
  partageable, et ce qu'il faut savoir avant de le transmettre.
- **Sans confirmation** : l'outil est en lecture seule, comme le reste de
  l'atelier. Le membre récupère son propre fichier, là où il vient de le
  demander.
- **Toujours en pièce jointe** côté navigateur (`Content-Disposition:
  attachment`) : rien n'est jamais interprété, même un HTML produit dans
  l'atelier.
- **Coupé par la désactivation du plugin** : l'activation est relue à
  chaque accès, comme pour les pages publiques.

Les octets ne sont ni copiés ni stockés : ils traversent depuis LeaSH
jusqu'au navigateur, par tranches d'un mégaoctet. Un fichier de 200 Mo ne
coûte donc jamais sa taille en mémoire, quel que soit le nombre de
téléchargements simultanés.

**Limite connue** : les requêtes `Range` ne sont pas servies. Un
téléchargement interrompu repart de zéro, et le lien ne permet pas de lire
une vidéo en streaming dans le navigateur — seulement de la télécharger.
Le jeton voyage dans le chemin de l'URL, donc dans les journaux du reverse
proxy : c'est la contrepartie assumée d'une durée de 24 h.

## Documents

Ces recettes ne sont plus dans le prompt du sous-agent : elles vivent
dans la compétence `edit-office-document`, que l'agent charge quand la
tâche s'y prête (voir [skills.md](skills.md)). Le prompt ne garde que le
contrat de l'atelier.

La chaîne bureautique tient en trois outils, que la compétence enchaîne
dans cet ordre :

| Besoin                          | Commande                                       |
| ------------------------------- | ---------------------------------------------- |
| Lire un docx, odt ou pdf        | `pandoc rapport.docx -t markdown`, `pdftotext rapport.pdf -` |
| Écrire ou modifier              | en markdown, puis `pandoc brouillon.md -o rapport.docx` |
| Produire un PDF                 | `office-convert pdf rapport.docx`              |
| Vérifier un rendu               | `pdftoppm -png -r 60 -f 1 -l 1 rapport.pdf page` puis `view_file` |
| Rendre un scan cherchable       | `ocrmypdf --image-dpi 300 -l fra scan.pdf sortie.pdf` |
| Nettoyer les métadonnées        | `exiftool -all= -overwrite_original photo.jpg` |

`office-convert` est le point d'entrée LibreOffice fourni par l'image ;
`soffice` n'est pas autorisé en direct, car invoqué sans garde-fous il écrit
son profil utilisateur dans le workspace du membre.

**La limite à connaître** : éditer un document reçu passe par une
conversion en markdown, puis un retour au format d'origine. Le texte
survit, la mise en page d'un document richement formaté non. La
compétence demande à l'agent de le dire plutôt que de dégrader
silencieusement le document — mais c'est une limite réelle, pas un défaut
à corriger par un réglage.

## Le service LeaSH (« toolbox »)

L'image est produite par `Dockerfile.toolbox` du dépôt LeaSH : le serveur
MCP multi-tenant, `bubblewrap`, `ffmpeg` et `imagemagick`. Elle est déployée
comme une application Dokku distincte, **sans aucune exposition publique**
(`proxy:disable`, aucun domaine), jointe à Automata par un réseau Dokku
interne.

Bubblewrap a besoin de trois assouplissements du conteneur pour créer ses
namespaces :

```
--security-opt seccomp=unconfined
--security-opt apparmor=unconfined
--security-opt systempaths=unconfined
```

**Débit** : le limiteur de LeaSH compte par IP cliente (60 requêtes par
minute par défaut). Automata parle depuis une seule adresse pour tous ses
membres, et chaque commande ouvre sa propre session MCP : le défaut se
tarit au milieu d'une tâche un peu longue. L'application est configurée
avec `LEASH_HTTP_RATE_LIMIT=600/minute` et `LEASH_HTTP_BURST=120` — le
service n'étant joignable que par Automata sur le réseau interne, ce qui
borne réellement le travail est ailleurs : sérialisation par workspace,
`max_duration` de 300 s et quota de 256 Mio.

Sans eux, `bwrap` échoue successivement sur la création du user namespace,
sur `mount --make-rslave /`, puis sur le montage de `/proc`. Si un
exploitant ne peut pas les accorder, LeaSH accepte
`LEASH_SANDBOX_BACKEND=chroot` — mais ce repli ne coupe pas le réseau et ne
monte rien : le conteneur redevient alors la seule frontière, ce qui
invalide la décision de sécurité ci-dessus.

Les workspaces sont réapés après `LEASH_TTL` (24 h) d'inactivité, avec un
quota disque par workspace. Les fichiers d'un membre ne survivent donc pas
à une journée sans usage : c'est un atelier, pas un stockage.

## Essai de bout en bout

Le banc `internal/e2e` éprouve la chaîne complète — vrai sous-processus de
plugin, vrai serveur LeaSH, vrai `ffmpeg` — contre un conteneur toolbox
local :

```bash
docker run -d --name leash-lab \
  --security-opt seccomp=unconfined \
  --security-opt apparmor=unconfined \
  --security-opt systempaths=unconfined \
  -p 18443:8443 \
  -e LEASH_HMAC_SECRET=<48 octets aléatoires> \
  -e LEASH_APIKEY_AUTOMATA=<48 octets aléatoires> \
  -e LEASH_APIKEY_AUTOMATA_POLICY=/app/policies/toolbox.yaml \
  -e LEASH_TTL=24h \
  leash-toolbox:dev

AUTOMATA_E2E_LEASH_URL=http://127.0.0.1:18443 \
AUTOMATA_E2E_LEASH_KEY=<la même clé> \
AUTOMATA_E2E_FIXTURE=/chemin/vers/une-video.mp4 \
go test -v ./internal/e2e/
```

Sans ces variables, le test est ignoré : la suite du dépôt ne dépend ni de
Docker ni du réseau.
