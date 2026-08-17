# Installation

Ce guide part d'une machine vierge et s'arrête quand Automata répond à un
premier message. Comptez une heure la première fois, dont la moitié à
rassembler les jetons d'API et les identifiants de canaux.

## 1. Prérequis

| Élément | Version | Pourquoi |
|---|---|---|
| Go | 1.26 ou plus | Compilation. Inutile si vous partez de l'image Docker |
| SQLite | aucun | Le driver est en Go pur, rien à installer |
| Un compte WhatsApp | | Le premier démarrage demande de lier un appareil |
| Un fournisseur LLM | | OpenAI, Mistral ou OpenRouter |
| Un fournisseur de transcription | | Requis seulement si vous activez l'audio |
| Des serveurs MCP | | Requis seulement pour les spécialistes qui en déclarent |

Le modèle doit accepter les images si vous activez les pièces jointes, et
savoir appeler des outils. Sans tool calling, la délégation et la mémoire ne
fonctionnent pas.

### Dépendances locales non publiées

Automata dépend de trois bibliothèques qui n'ont pas encore de version
publiée : `go-courier`, `genai` et `amoxtli`. Le `go.mod` les redirige vers
des répertoires frères :

```text
workspace/
├── automata/
├── go-courier/
├── genai/
└── amoxtli/
```

Clonez les quatre dépôts côte à côte, sinon la compilation échoue dès
`go mod download`. C'est aussi la raison pour laquelle la construction de
l'image Docker exige des contextes de build supplémentaires, décrits dans
[deployment.md](deployment.md).

## 2. Compiler

```bash
cd automata
go build -o automata ./cmd/automata
./automata --help
```

`CGO_ENABLED=0` fonctionne : le driver SQLite est du Go pur compilé en WASM,
et le binaire produit est statique.

## 3. Préparer la configuration

Deux voies. Le wizard pose des questions et écrit le fichier pour vous. La
copie de l'exemple convient si vous préférez éditer directement.

### Le wizard

```bash
./automata config init
```

Il vous demande le nom de l'organisation, le fournisseur de modèle, les
fonctionnalités à activer (audio, pièces jointes, observabilité), les
personnes autorisées, leurs canaux, les spécialistes à brancher et,
éventuellement, une tâche planifiée. Validez par Entrée pour accepter la
valeur entre crochets.

Il écrit deux fichiers :

| Fichier | Contenu |
|---|---|
| `config/config.yaml` | La configuration, commentée |
| `config/config.env` | La liste des variables d'environnement à renseigner |

Aucun secret n'entre dans le YAML. Le wizard produit des références
d'environnement et rassemble leurs noms dans le second fichier, qu'il ne vous
reste qu'à remplir.

Il refuse d'écraser un fichier existant. Pour choisir d'autres emplacements :

```bash
./automata config init -output /etc/automata/config.yaml -env-output /etc/automata/automata.env
```

Une question mérite d'être anticipée. Quand vous activez un spécialiste dont
le serveur MCP demande un jeton, il vous demande si chaque personne a le sien.
Répondre oui génère une surcharge par principal, ce qui donne à chacun sa
propre connexion, isolée des autres. C'est ce qu'il faut pour un agenda
personnel. Répondre non met un jeton unique au niveau du serveur, ce qui
convient à une recherche web. Le détail est dans [agents.md](agents.md).

Le wizard couvre l'ossature. Les réglages fins, une seconde organisation, un
spécialiste maison, plusieurs groupes, se font ensuite dans le fichier, avec
[configuration.md](configuration.md) sous la main.

### Ou partir de l'exemple

Le dépôt fournit une configuration complète et commentée.

```bash
cp config/config.example.yaml config/config.yaml
```

`config/config.yaml` est ignoré par git. Aucun secret n'y figure : les valeurs
sensibles sont référencées par variables d'environnement et lues au
chargement.

Un piège à connaître tout de suite. L'expansion des variables s'applique au
fichier entier, commentaires compris. Écrire une référence d'environnement
dans un commentaire, même à titre d'exemple, fait échouer le chargement si la
variable n'existe pas.

### Les chemins

Les chemins relatifs sont résolus depuis le répertoire du fichier de
configuration, pas depuis le répertoire courant. L'exemple livré vise les
prompts par `../prompts/`, ce qui tombe juste aussi bien depuis le dépôt que
dans le conteneur, où `/config` et `/prompts` sont deux volumes distincts.

Pour une exécution locale, remplacez les chemins de données absolus par des
chemins relatifs :

```yaml
storage:
  application:
    path: ../data/app.sqlite
memory:
  store:
    path: ../data/amoxtli.sqlite
  indexes:
    - id: lexical
      type: bleve
      path: ../data/memory.bleve
      weight: 1
courier:
  providers:
    whatsapp:
      session_path: ../data/courier/whatsapp
```

Créez les répertoires avant le premier démarrage :

```bash
mkdir -p data/courier
```

## 4. Renseigner l'environnement

Si vous avez utilisé le wizard, la liste est déjà dans `config/config.env` :
remplissez chaque valeur, puis chargez le fichier.

```bash
set -a && . ./config/config.env && set +a
```

Sinon, listez ce que votre configuration référence et exportez chaque
variable. Pour l'exemple livré :

```bash
export MAIN_MODEL=gpt-4o
export MAIN_API_KEY=sk-...
export MAIN_BASE_URL=https://api.openai.com/v1
export TRANSCRIPTION_MODEL=whisper-1
export TRANSCRIPTION_API_KEY=sk-...

export GOOGLE_CALENDAR_MCP_URL=https://...
export GOOGLE_CALENDAR_MCP_TOKEN=...
export SEARCH_MCP_URL=https://...
export TODO_MCP_URL=https://...

export ALICE_WHATSAPP_ID='33612345678@s.whatsapp.net'
export LEO_WHATSAPP_ID='33698765432@s.whatsapp.net'
export ORG_GROUP_CHANNEL_ID='120363...@g.us'
export ALICE_PRIVATE_CHANNEL_ID='33612345678@s.whatsapp.net'
export ORG_GROUP_CALENDAR_ID=...
export ORG_GROUP_TODO_ID=...
export ALICE_CALENDAR_ID=...
export ALICE_TODO_ID=...
```

Une variable référencée mais absente est une erreur de démarrage, jamais une
chaîne vide. C'est voulu : une clé d'API silencieusement vide produirait des
erreurs incompréhensibles bien plus tard.

### Trouver les identifiants WhatsApp

Vous ne les connaissez pas avant le premier démarrage. Procédez en deux temps.

Mettez d'abord des valeurs provisoires, démarrez, liez l'appareil, envoyez un
message depuis chaque compte concerné. Les messages seront refusés, et c'est
exactement ce qui vous donne l'information : chaque refus est journalisé avec
le `channel_id` réellement reçu.

```json
{"level":"INFO","msg":"ingress: message ignoré (identité non résolue ou non autorisée)","provider":"whatsapp","channel_id":"33612345678@s.whatsapp.net"}
```

Reportez ces valeurs dans votre environnement, redémarrez. Pour un groupe,
l'identifiant se termine par `@g.us`.

## 5. Valider avant de démarrer

```bash
./automata config validate -config config/config.yaml
```

La commande refuse la configuration sur la moindre variable absente,
référence inconnue, expression cron invalide, fuseau horaire inexistant ou
permission mal formée. Elle le fait avant toute connexion réseau, avant
d'ouvrir la base, avant de contacter WhatsApp.

Prenez l'habitude de la lancer après chaque modification. Elle agrège toutes
les erreurs en une fois plutôt que de s'arrêter à la première.

## 6. Démarrer

```bash
./automata -config config/config.yaml
```

Notez le tiret simple. Le drapeau suit la convention Go, pas celle des
doubles tirets.

Au premier lancement, go-courier affiche un QR code à scanner depuis WhatsApp
(Appareils liés, Lier un appareil). La session est ensuite conservée dans
`session_path` et le QR code ne réapparaît plus, sauf si vous supprimez ce
répertoire ou déliez l'appareil.

Les journaux sont en JSON sur la sortie d'erreur :

```json
{"level":"INFO","msg":"automata starting"}
{"level":"INFO","msg":"mcp: connexion établie","server":"google-calendar","session":"whatsapp:33612345678@s.whatsapp.net"}
```

### Découvrir les identifiants de canaux et de comptes

Un identifiant de groupe ou de conversation privée est attribué par le
fournisseur : impossible de le connaître avant qu'un premier message n'en
provienne. Tout message venu d'une origine non déclarée est ignoré, mais ses
identifiants sont journalisés — c'est là qu'on lit les valeurs à reporter dans
`identities` et `channels` :

```json
{"level":"INFO","msg":"ingress: message ignoré (identité non résolue ou non autorisée)",
 "provider":"whatsapp","channel_id":"120363000000000000@g.us","channel_kind":"group",
 "channel_name":"Famille","user_id":"33612345678@s.whatsapp.net","user_name":"Alice"}
```

Écrivez donc au bot en privé, puis dans le groupe visé (en le mentionnant ou
non, peu importe : la règle de mention ne s'applique qu'aux canaux déjà
déclarés), et relevez les `channel_id` et `user_id` obtenus.

La configuration doit rester valide pour que le worker démarre et journalise :
si `channels` est encore vide de valeurs réelles, déclarez temporairement un
seul canal privé avec un identifiant fictif, le temps de cette découverte.
Deux canaux au même `channel_id` — deux variables d'environnement vides, par
exemple — sont refusés au chargement.

## 7. Vérifier

Envoyez un message privé depuis un compte déclaré dans `origins`. Vous devez
recevoir une réponse.

Dans un groupe, mentionnez l'assistant. Sans mention, le message est ignoré,
sans appel au modèle et sans trace autre qu'un compteur.

Si l'observabilité est activée :

```bash
curl -s http://127.0.0.1:9090/healthz/ready   # 200 quand le service est prêt
curl -s http://127.0.0.1:9090/metrics | jq    # compteurs agrégés, aucun contenu privé
./automata healthcheck                        # code de sortie 0 ou 1
```

## 8. Quand rien ne répond

Reprenez dans cet ordre, c'est presque toujours l'un de ces quatre.

L'origine n'est pas déclarée. Cherchez `identité non résolue` dans les
journaux : le `channel_id` affiché est celui à déclarer.

Le message vient d'un groupe sans mention. Comportement normal. Le compteur
`messages_ignored_no_mention` s'incrémente.

Le canal n'est pas déclaré. Un message venant d'un canal absent de `channels`
est refusé même si l'auteur est connu.

Le modèle refuse la requête. Regardez le niveau `ERROR`. Une pièce jointe
d'un type que le fournisseur n'accepte pas fait échouer la requête entière :
vérifiez `attachments.accepted_types` contre ce que le modèle admet
réellement.

## 9. En conteneur

L'image se construit et se déploie autrement, à cause des dépendances non
publiées. Tout est dans [deployment.md](deployment.md) : commande `buildx`
avec les contextes supplémentaires, volumes à monter, propriétaire du volume
de données, sonde de santé, arrêt gracieux.

## Suite

Ouvrez [configuration.md](configuration.md) pour la référence de chaque
section, ou [agents.md](agents.md) si votre besoin immédiat est d'ajouter un
spécialiste.
