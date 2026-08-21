# Plugin Workspace

Le plugin `workspace` donne à Automata un **atelier de fichiers** : un
membre envoie une vidéo ou une image par sa messagerie, demande une
opération dessus (« recadre cette vidéo », « retire le bas de l'image »),
et reçoit le fichier transformé en pièce jointe de la réponse.

Le travail se fait avec `ffmpeg` et `imagemagick`, dans un bac à sable
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
5. Il appelle `attach_file` : l'hôte récupère le résultat et le joint à la
   réponse.

Les octets ne traversent jamais la conversation : le modèle ne voit que des
chemins, des noms et des tailles.

**Le fichier doit accompagner la demande.** L'import ne cherche que dans le
message du tour courant. Si l'utilisateur envoie la vidéo puis, dans un
second message, demande le traitement, l'agent lui demandera de renvoyer le
fichier avec sa demande.

## Pourquoi les commandes ne demandent pas de confirmation

Les outils d'exécution sont déclarés `read_only`, à dessein. Une commande
lancée dans le bac à sable — réseau coupé, seul le workspace du membre
monté en écriture — n'a d'effet que sur les fichiers de ce membre. La
frontière de sécurité est le bac à sable, pas la confirmation ; exiger
« confirmer » à chaque commande `ffmpeg` rendrait l'agent inutilisable.

L'invariant du dépôt reste entier : tout outil qui écrit **hors** du bac à
sable passe par une action proposée. Rien ici n'écrit hors du bac à sable.

**Corollaire** : le réseau ne doit jamais être ouvert dans la policy de ce
bac à sable sans revenir sur cette décision.

## Configuration

Deux variables d'environnement, héritées de l'hôte — il n'y a rien à régler
par membre :

| Variable            | Description                                                    |
| ------------------- | -------------------------------------------------------------- |
| `LEASH_SERVER_URL`  | URL du serveur LeaSH, sur le réseau interne (jamais publique). |
| `LEASH_API_KEY`     | Clé API de l'instance Automata auprès de ce serveur.           |

Côté `config.yaml` :

```yaml
attachments:
  tool_types:
    - video/mp4
    - video/webm
    - video/quicktime
  max_tool_size: 16MiB
```

`max_tool_size` borne les pièces entrantes **et** les fichiers renvoyés.
16 Mio correspond à la limite d'une vidéo WhatsApp ; le prompt du
sous-agent lui demande de ré-encoder ses sorties sous ~15 Mio.

Le plugin s'active ensuite par organisation, dans l'onglet Plugins de la
fiche organisation.

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
