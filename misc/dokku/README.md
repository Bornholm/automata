# Déploiement Dokku

Automata tourne en un process unique, sans interface web. Il consomme les
messages WhatsApp, appelle le modèle, et déclenche ses tâches planifiées. Rien
n'est exposé publiquement : l'application est déclarée en **worker**, pas en
web, donc Dokku ne lui route aucun trafic HTTP, ne lui attribue aucun domaine
et ne tente aucune vérification.

## Préparation, dans l'ordre

```bash
make dokku-setup      # créer l'app, déclarer les volumes
make dokku-storage    # créer les répertoires et leur propriétaire (accès admin)
make dokku-env        # pousser les secrets
make dokku-config     # déposer config.yaml
make dokku-deploy     # premier déploiement
make dokku-scale      # démarrer le worker
make dokku-qr         # scanner le QR code de liaison WhatsApp
```

Adaptez d'abord les variables en tête de `dokku.mk` si votre instance diffère :

| Variable | Défaut | Rôle |
|---|---|---|
| `DOKKU_APP` | `automata` | Nom de l'application |
| `DOKKU_HOST` | `dokku.example.org` | Hôte Dokku |
| `DOKKU_SSH_ADMIN` | `root@$(DOKKU_HOST)` | Compte pour les opérations hors périmètre dokku |
| `DOKKU_STORAGE` | `/var/lib/dokku/data/storage/$(DOKKU_APP)` | Volume persistant côté hôte |
| `DOKKU_UID` | `65532` | Propriétaire des volumes, utilisateur de l'image distroless |
| `DOKKU_ENV_FILE` | `config/config.env` | Fichier lu par `dokku-env` |
| `DOKKU_CONFIG_FILE` | `config/config.yaml` | Fichier déposé par `dokku-config` |

Toutes se surchargent en ligne de commande :

```bash
make dokku-deploy DOKKU_APP=automata-test DOKKU_HOST=dokku.exemple.net
```

## Ce que fait chaque étape

### dokku-setup

Crée l'application, efface un éventuel chemin de Dockerfile hérité, et déclare
deux montages :

| Montage | Contenu |
|---|---|
| `/data` | Bases SQLite, index de mémoire, session WhatsApp |
| `/config` | `config.yaml` |

Les prompts, eux, sont versionnés et voyagent dans l'image.

La cible est idempotente, vous pouvez la relancer. Elle affiche les montages
en place à la fin : il doit y en avoir exactement deux.

### dokku-storage

Crée `data/`, `data/courier/` et `config/` sous le volume, puis leur donne
l'UID 65532 pour propriétaire.

Deux raisons à cette étape séparée. Le compte `dokku` n'accepte que des
commandes dokku, un `mkdir` ou un `chown` demande donc un accès
administrateur, d'où `DOKKU_SSH_ADMIN`. Et aucune des valeurs proposées par
`storage:ensure-directory --chown` ne correspond à l'utilisateur nonroot de
l'image distroless.

Sans cette étape, le premier démarrage échoue sur une écriture refusée.
`courier/` doit exister avant : go-courier y écrit la session WhatsApp et ne
crée pas l'arborescence lui-même.

### dokku-env

Lit `config/config.env`, le fichier que produit `automata config init`, et
pousse chaque variable renseignée. Les valeurs vides sont ignorées, pour ne
pas écraser par du vide une clé déjà en place. Seuls les noms sont affichés,
ce fichier contenant des secrets.

`--no-restart` : la configuration est poussée avant le premier déploiement, il
n'y a rien à redémarrer.

Une variable référencée par `config.yaml` mais absente est une erreur de
démarrage, jamais une chaîne vide. Un envoi incomplet se voit donc
immédiatement dans les journaux.

### dokku-config

Copie `config/config.yaml` sur le volume et lui donne le bon propriétaire. Ce
fichier n'est pas versionné, il décrit un déploiement précis, et ne contient
aucun secret : les valeurs sensibles y sont des références d'environnement.

Générez-le et validez-le avant de l'envoyer :

```bash
automata config init -output config/config.yaml
automata config validate -config config/config.yaml
```

Les chemins doivent viser les volumes montés : `/data/app.sqlite`,
`/data/amoxtli.sqlite`, `/data/memory.bleve`, `/data/courier/whatsapp`, et les
prompts `/prompts/*.md`.

### dokku-deploy

Pousse HEAD, augmenté des dépendances vendorisées.

`go.mod` redirige `go-courier`, `genai` et `amoxtli` vers des répertoires
frères du dépôt, faute de version publiée. Dokku construit l'image sur son
serveur à partir du seul dépôt poussé, où ces répertoires n'existent pas : le
build échouerait dès `go mod download`.

La cible vendorise donc les dépendances, fabrique un commit contenant HEAD
plus `vendor/`, et pousse ce commit. Les 85 Mio produits n'entrent jamais dans
l'historique local : le commit est construit dans un index temporaire, poussé,
puis abandonné. Votre dépôt de travail n'est pas touché, pas même son index.

Le commit emporte aussi `misc/dokku/Dockerfile` et `misc/dokku/Procfile`,
placés à la racine sous les noms que Dokku attend. Aucune configuration de
builder côté serveur.

Le jour où les trois bibliothèques publient une version taguée, tout ceci se
réduit à un `git push`.

### dokku-scale

Démarre le worker. À lancer après le premier déploiement seulement : Dokku ne
connaît les process qu'une fois l'image construite.

Ne dépassez jamais 1. La persistance est mono-écrivain et le scheduler
s'appuie sur des verrous en mémoire, propres au processus. Deux instances
partageant `/data` corrompraient la base ou exécuteraient deux fois les tâches
planifiées.

### dokku-qr

Au premier démarrage, go-courier affiche un QR code à scanner depuis WhatsApp
(Appareils liés, Lier un appareil). Il n'apparaît que dans les journaux.

Un QR code expire vite. S'il n'est plus valable, redémarrez pour en obtenir un
autre :

```bash
ssh dokku@dokku.example.org ps:restart automata
```

La session est ensuite conservée dans `/data/courier/` et le QR code ne
réapparaît plus tant que ce volume survit. C'est la raison pour laquelle
`/data` doit être un volume : sans lui, chaque déploiement reconstruirait le
conteneur et redemanderait un scan.

## Exploitation

```bash
make dokku-logs          # journaux en continu
make dokku-ps            # état des process
make dokku-healthcheck   # sonde de santé, code de sortie 0 ou 1
```

`dokku-healthcheck` suppose `observability.enabled: true` dans la
configuration. L'image étant une distroless sans shell, `dokku enter` n'ouvre
aucun interpréteur : seul le binaire lui-même peut être exécuté.

## Valider avant de pousser

```bash
make dokku-build
```

Construit l'image exactement comme Dokku le fera, avec le même Dockerfile et
les mêmes dépendances vendorisées. Utile pour vérifier une modification du
Dockerfile sans attendre un déploiement.

## Sauvegarde

Tout l'état tient dans un répertoire :

```bash
ssh root@dokku.example.org
tar czf automata-backup.tar.gz -C /var/lib/dokku/data/storage/automata .
```

Arrêtez l'application avant de copier, ou acceptez une copie potentiellement
incohérente : SQLite est en mode WAL, les fichiers `-wal` et `-shm` font
partie de l'état et doivent être copiés avec la base, jamais séparément. La
procédure détaillée est dans [operations.md](../../docs/operations.md).

Attention à la taille si les pièces jointes sont conservées
(`attachments.max_history`) : `app.sqlite` porte alors les images reçues.
