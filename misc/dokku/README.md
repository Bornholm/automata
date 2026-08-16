# Déploiement Dokku

Automata tourne en un process unique, sans interface web. Il consomme les
messages WhatsApp, appelle le modèle, et déclenche ses tâches planifiées. Rien
n'est exposé publiquement : l'application est déclarée en **worker**, pas en
web, donc Dokku ne lui route aucun trafic HTTP.

```bash
make dokku-deploy
```

## Ce que fait la cible, et pourquoi elle n'est pas un simple git push

`go.mod` redirige `go-courier`, `genai` et `amoxtli` vers des répertoires
frères du dépôt, faute de version publiée. Dokku construit l'image sur son
serveur à partir du seul dépôt poussé, où ces répertoires n'existent pas : le
build échouerait dès `go mod download`.

La cible vendorise donc les dépendances, fabrique un commit qui contient HEAD
plus `vendor/`, et pousse ce commit. Les 85 Mio produits n'entrent jamais dans
l'historique local : le commit est construit dans un index temporaire, poussé,
puis abandonné. Votre dépôt de travail n'est pas touché, pas même son index.

Le commit poussé emporte aussi `misc/dokku/Dockerfile` et
`misc/dokku/Procfile`, placés à la racine sous les noms que Dokku attend.
Aucune configuration de builder n'est donc nécessaire côté serveur.

Le jour où les trois bibliothèques publient une version taguée, tout ceci se
réduit à un `git push`.

## Premier déploiement

```bash
ssh dokku@dokku.example.org apps:create automata
```

### 1. Volumes persistants

Un conteneur est reconstruit à chaque déploiement. Tout ce qui n'est pas sur
un volume monté disparaît, **y compris la liaison WhatsApp** : sans volume,
chaque déploiement vous redemanderait de scanner un QR code.

```bash
ssh dokku@dokku.example.org storage:ensure-directory automata
ssh dokku@dokku.example.org storage:mount automata /var/lib/dokku/data/storage/automata/data:/data
ssh dokku@dokku.example.org storage:mount automata /var/lib/dokku/data/storage/automata/config:/config
```

`/data` porte les bases SQLite, l'index de mémoire et la session WhatsApp.
`/config` porte `config.yaml`, qui n'est pas versionné puisqu'il décrit un
déploiement précis. Les prompts, eux, sont dans l'image.

Le processus tourne sous l'UID 65532 (utilisateur `nonroot` de l'image
distroless). Les répertoires montés doivent lui appartenir, sinon le premier
démarrage échoue sur une écriture refusée :

```bash
ssh dokku@dokku.example.org
sudo mkdir -p /var/lib/dokku/data/storage/automata/data/courier
sudo chown -R 65532:65532 /var/lib/dokku/data/storage/automata
```

Le sous-répertoire `courier/` doit exister avant le premier démarrage.

### 2. Déposer la configuration

Générez-la localement, puis copiez-la sur le serveur :

```bash
automata config init -output config/config.yaml
automata config validate -config config/config.yaml

scp config/config.yaml dokku@dokku.example.org:/var/lib/dokku/data/storage/automata/config/
```

Dans cette configuration, les chemins doivent viser les volumes montés :
`/data/app.sqlite`, `/data/amoxtli.sqlite`, `/data/memory.bleve`,
`/data/courier/whatsapp`, et les prompts `/prompts/*.md`.

### 3. Variables d'environnement

Aucun secret ne figure dans `config.yaml` : il ne contient que des références
d'environnement. Le fichier `config.env` produit par `config init` en donne la
liste exacte.

```bash
ssh dokku@dokku.example.org config:set automata \
  MAIN_MODEL='gpt-4o' \
  MAIN_API_KEY='...' \
  MAIN_BASE_URL='https://api.openai.com/v1' \
  TRANSCRIPTION_MODEL='whisper-1' \
  TRANSCRIPTION_API_KEY='...' \
  ALICE_WHATSAPP_ID='33612345678@s.whatsapp.net' \
  ALICE_PRIVATE_CHANNEL_ID='33612345678@s.whatsapp.net'
```

Une variable référencée mais absente est une erreur de démarrage, jamais une
chaîne vide. Un `config:set` incomplet se voit donc immédiatement dans les
journaux.

### 4. Déployer et démarrer le worker

```bash
make dokku-deploy
ssh dokku@dokku.example.org ps:scale automata worker=1
```

L'application n'ayant pas de process `web`, Dokku ne lui attribue aucun
domaine et ne tente aucune vérification HTTP.

### 5. Lier WhatsApp

Au premier démarrage, go-courier affiche un QR code à scanner. Il est dans les
journaux :

```bash
make dokku-qr
```

Scannez depuis WhatsApp (Appareils liés, Lier un appareil). La session est
ensuite conservée dans `/data/courier/`, et le QR code ne réapparaît plus tant
que ce volume survit.

Un QR code expire vite. S'il n'est plus valable, redémarrez pour en obtenir un
autre :

```bash
ssh dokku@dokku.example.org ps:restart automata
```

## Exploitation

```bash
make dokku-logs          # journaux en continu
make dokku-healthcheck   # sonde de santé, code de sortie 0 ou 1
```

`make dokku-healthcheck` suppose `observability.enabled: true` dans la
configuration. L'image étant une distroless sans shell, `dokku enter` n'ouvre
aucun interpréteur : seul le binaire lui-même peut être exécuté, ce que fait
cette cible.

## Sauvegarde

Tout l'état tient dans un répertoire :

```bash
ssh dokku@dokku.example.org
tar czf automata-backup.tar.gz -C /var/lib/dokku/data/storage/automata .
```

Arrêtez l'application avant de copier, ou acceptez une copie potentiellement
incohérente : SQLite est en mode WAL, les fichiers `-wal` et `-shm` font partie
de l'état et doivent être copiés avec la base, jamais séparément. La procédure
détaillée est dans [operations.md](../../docs/operations.md).

Attention à la taille si les pièces jointes sont conservées
(`attachments.max_history`) : `app.sqlite` porte alors les images reçues.

## Une seule instance

Ne montez jamais `automata` au-delà d'un worker. La persistance est
mono-écrivain et le scheduler s'appuie sur des verrous en mémoire, propres au
processus. Deux instances partageant `/data` corrompraient la base ou
exécuteraient deux fois les tâches planifiées.

```bash
ssh dokku@dokku.example.org ps:scale automata worker=1   # jamais plus
```
