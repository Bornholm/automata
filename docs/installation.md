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

### Dépendances

Toutes les dépendances sont des modules Go publiés (`go-courier`, `genai`,
`amoxtli` compris) : `go mod download` suffit, aucun dépôt frère à cloner.
Seul le SDK de plugins (`pkg/pluginsdk`) est redirigé vers le sous-répertoire
du dépôt, ce qui est transparent.

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

Le spécialiste de recherche a besoin d'un serveur MCP en face. Si vous n'en
avez pas, [misc/web-search](../misc/web-search/README.md) en construit un :
une instance SearXNG privée et son serveur MCP dans un conteneur, démarrés
par `make web-search-up`, à brancher sur `SEARCH_MCP_URL` et
`SEARCH_MCP_TOKEN`.

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
```

Créez le répertoire de données avant le premier démarrage :

```bash
mkdir -p data
```

Les comptes de messagerie et les modèles ne figurent pas dans ce fichier :
ils se créent en ligne, dans l'administration, après le premier démarrage
(section 6). Le fichier ne décrit que la machine — stockage, secrets, ports,
agents.

## 4. Renseigner l'environnement

Si vous avez utilisé le wizard, la liste est déjà dans `config/config.env` :
remplissez chaque valeur, puis chargez le fichier.

```bash
set -a && . ./config/config.env && set +a
```

Sinon, listez ce que votre configuration référence et exportez chaque
variable. Pour l'exemple livré, ce sont les secrets du serveur web et les
serveurs MCP éventuels :

```bash
export WEB_SESSION_SECRET=$(openssl rand -hex 32)   # scelle aussi les secrets en base
export WEB_ADMIN_PASSWORD_HASH='$2a$10$...'         # bcrypt, voir section 6
export STORAGE_ENCRYPTION_KEY=$(openssl rand -hex 32)

export SEARCH_MCP_URL=http://127.0.0.1:3000/mcp   # voir misc/web-search
export SEARCH_MCP_TOKEN=...
```

Les clés d'API des modèles ne sont **pas** des variables d'environnement :
elles se saisissent dans l'administration et sont scellées en base. Perdre
`WEB_SESSION_SECRET` ou `STORAGE_ENCRYPTION_KEY` rend illisible ce qu'ils
protègent — sauvegardez-les à part.

Une variable référencée mais absente est une erreur de démarrage, jamais une
chaîne vide. C'est voulu : une clé d'API silencieusement vide produirait des
erreurs incompréhensibles bien plus tard.

### Rattacher les personnes et les groupes

Vous n'avez plus d'identifiants WhatsApp à recopier. Les membres et les
groupes se rattachent **par jeton** : l'administration génère un jeton
(`atm_…`) pour un membre pré-créé ou pour un groupe, la personne l'envoie à
l'assistant depuis sa messagerie, et le rattachement est fait. Les sections
`identities` et `channels` du fichier restent supportées pour un usage local
ou un import manuel (`automata web bootstrap`), mais ne sont plus le chemin
normal.

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

Au premier lancement, rien n'écoute encore aucune messagerie : ouvrez
l'administration (`web.addr`, identifiants de `web.admin`), écran « Canaux
et plateformes », et créez votre premier compte. Pour WhatsApp, le QR code à
scanner (Appareils liés, Lier un appareil) s'affiche directement dans cet
écran ; la session est ensuite conservée sur le disque et le QR code ne
réapparaît plus, sauf si vous déliez l'appareil.

Puis, écran « Modèles » : ajoutez au moins un client (fournisseur, modèle,
clé d'API) et affectez-le aux rôles d'instance — `main` au minimum. Sans
modèle sur un rôle, la fonction correspondante reste muette et le dit dans
les journaux.

Les journaux sont en JSON sur la sortie d'erreur :

```json
{"level":"INFO","msg":"automata starting"}
{"level":"INFO","msg":"mcp: connexion établie","server":"google-calendar","session":"whatsapp:33612345678@s.whatsapp.net"}
```

### Rattacher un premier membre

Créez une organisation puis un membre dans l'administration, générez-lui un
jeton personnel, et envoyez ce jeton à l'assistant depuis WhatsApp : la
conversation privée est rattachée, l'assistant se présente, et propose une
courte visite d'accueil. Un jeton de groupe, envoyé dans un groupe où
l'assistant est présent, rattache ce groupe à l'organisation de la même
façon.

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
