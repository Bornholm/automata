# Plugin Sous-agents

Le plugin `subagents` donne à chaque membre un catalogue de spécialistes à
activer depuis son profil. Chaque entrée du catalogue est un sous-agent
complet — son prompt, sa description, et les serveurs MCP dont il tire ses
outils. Une fois activée, elle apparaît à l'orchestrateur comme n'importe
quel spécialiste : `delegate_to_netprobe`, `delegate_to_<autre>`.

Le catalogue est un fichier YAML, écrit par l'exploitant. Le membre ne fait
que deux choses : choisir dans ce qui lui est proposé, et renseigner ses
propres identifiants.

## Le modèle de confiance, en une phrase

**Déposer une entrée dans le catalogue engage la même confiance que déposer
un binaire dans le répertoire des plugins.** Une entrée peut télécharger et
lancer un processus sur le serveur, avec les droits d'Automata et sans bac
à sable — contrairement à l'atelier `workspace`, qui a LeaSH. Le fichier
doit rester sous le contrôle exclusif de l'exploitant.

Trois choses ne viennent JAMAIS d'un membre ni du modèle : une URL, une
commande, un en-tête. Le seul apport du membre est la valeur d'un
identifiant qu'il possède déjà.

## Installation

1. Déposer le binaire `subagents` dans le répertoire des plugins
   (`plugins.dir`, voir [configuration.md](configuration.md)).
2. Renseigner `SUBAGENTS_DATA_DIR` dans l'environnement du service : c'est
   là que sont installés les serveurs MCP. Sur un déploiement Docker ou
   Dokku, un sous-répertoire du volume de données (`/data/subagents`).
   Sans cette variable, les entrées à installer sont désactivées et
   l'interface le dit ; les entrées à serveur HTTP continuent de
   fonctionner.
3. `SUBAGENTS_CATALOG` pointe sur le catalogue de l'exploitant. Absent, le
   catalogue embarqué s'applique — il propose `netprobe` (voir plus bas).
   Le fichier de l'exploitant **remplace** le catalogue embarqué, il ne
   s'y ajoute pas.
4. L'administrateur active le plugin pour l'organisation (fiche de
   l'organisation → onglet Plugins).
5. Chaque membre ouvre l'onglet du plugin dans son profil, renseigne les
   identifiants demandés s'il y en a, et active ce qui l'intéresse. Le
   sous-agent est disponible dès le message suivant.

| Variable | Rôle |
| --- | --- |
| `SUBAGENTS_DATA_DIR` | Répertoire d'installation des serveurs MCP. Absente : les entrées à installer sont désactivées. |
| `SUBAGENTS_CATALOG`  | Catalogue de l'exploitant. Absente : le catalogue embarqué. |

Un catalogue illisible ou invalide **arrête le plugin**, en nommant
l'entrée fautive. C'est délibéré : chargé à moitié, il laisserait un membre
activer un agent qui ne se montera jamais, sans que rien ne le dise.

## Le format du catalogue

```yaml
agents:
  - name: tickets              # devient delegate_to_tickets
    description: reads the ticket tracker and summarises what is open
    system_prompt: |           # anglais : ces deux textes partent au modèle
      You are a ticket assistant...
    max_tool_calls: 6          # 0 : défaut de l'hôte

    # Ce que le membre doit saisir. Les valeurs sont scellées par l'hôte,
    # jamais relues vers l'interface, jamais journalisées, jamais montrées
    # au modèle. Elles remplacent les patrons {{clé}} ci-dessous : c'est
    # cette injection qui cloisonne un membre d'un autre.
    credentials:
      - key: api_token
        label: Jeton d'API
        help: Réglages → Jetons personnels
        required: true         # l'entrée n'est pas montée tant qu'il manque

    servers:
      - name: tracker
        transport: streamable-http   # stdio | http | streamable-http
        url: https://tracker.example.org/mcp
        headers:
          Authorization: "Bearer {{api_token}}"
```

Un serveur `stdio` déclare une commande, éventuellement un environnement,
et la façon de l'installer :

```yaml
      - name: netprobe
        transport: stdio
        version: "0.4.0"
        install:
          fetch:
            url: https://example.org/releases/v{{version}}/outil_{{version}}_{{os}}_{{arch}}.tar.gz
            checksums: https://example.org/releases/v{{version}}/checksums.txt
            extract: outil       # le fichier à sortir de l'archive tar.gz
        command: ["{{bin}}/outil", "--config", "{{files}}/policy.yaml"]
        env:
          OUTIL_TOKEN: "{{api_token}}"
        files:                   # posés dans {{files}} avant le démarrage
          policy.yaml: |
            ...
        read_only: [probe, lookup]
```

Patrons disponibles : les clés de `credentials`, plus `{{bin}}`,
`{{files}}`, `{{version}}`, `{{os}}` et `{{arch}}`, fournis par le plugin.
Un patron sans valeur est une **erreur**, jamais un passage tel quel — et
le message d'erreur ne cite que le nom, jamais la valeur. Une clé
d'identifiant qui porterait le nom d'un patron réservé est refusée au
chargement.

`install` et `files` ne peuvent pas porter d'identifiant de membre : ils
sont posés une fois pour tous, et y mettre le jeton de quelqu'un le
placerait dans un fichier partagé. Le catalogue le refuse.

### Lecture ou écriture

Un outil s'exécute pendant le tour s'il est une **lecture**. Tout le reste
est une écriture : l'appel n'est jamais exécuté, il devient une action
proposée que la personne confirme d'un « confirmer » littéral, comme
partout ailleurs dans Automata.

L'annotation `readOnlyHint` du serveur MCP fait foi quand elle existe. À
défaut, la liste `read_only:` du catalogue, décidée par l'exploitant. Sans
l'un ni l'autre : **écriture**. Un outil qui ne s'annonce pas n'est pas un
outil inoffensif.

### Installation et mises à jour

`version` est l'identité de l'installation, pas une décoration : c'est en
changeant ce numéro dans le catalogue qu'on met un serveur à jour. Sans
lui, « installer si le binaire est absent » figerait le serveur au premier
jour, et une correction de sécurité publiée en amont ne serait jamais
reprise. Il est donc obligatoire dès qu'`install` est déclaré.

Ce qui se passe alors :

- chaque version vit dans son propre répertoire, `<données>/servers/<agent>/<serveur>/<version>/` ;
  un retour arrière est l'écriture de l'ancien numéro dans le catalogue,
  sans retéléchargement ;
- `installed.json` retient la version en service, sa somme et sa date :
  c'est ce qui permet d'annoncer « 0.4.0 installée, mise à jour vers 0.5.0
  en attente » dans la page de profil ;
- **le démarrage du plugin n'installe rien.** L'installation a lieu à la
  première utilisation qui suit. Un plugin qui téléchargerait au démarrage
  rendrait celui d'Automata dépendant d'un dépôt distant ;
- les connexions ouvertes sur l'ancienne version ne sont pas coupées en
  plein appel : elles sont retirées et fermées à leur retour au repos ;
- un échec de mise à jour ne supprime rien et laisse l'ancienne version en
  service. Dégrader vaut mieux qu'interrompre.

Le chemin par défaut est un **téléchargement vérifié** : le plugin va
chercher l'archive lui-même et refuse tout ce dont le sha256 ne correspond
pas au `checksums.txt` de la release (ou au `sha256:` écrit dans le
catalogue). Il fonctionne dans l'image distroless publiée, qui n'a ni shell
ni compilateur. Une somme qui ne correspond pas annule tout : rien n'est
écrit, l'entrée ne se monte pas, et la page de profil dit pourquoi.

Une commande libre reste déclarable pour les cas particuliers :

```yaml
        install:
          command: ["go", "install", "example.org/outil@v{{version}}"]
```

Elle exige une image qui embarque l'outillage : dans l'image distroless
publiée, elle échouera franchement. Sa sortie va aux journaux de
l'exploitant, jamais au modèle.

## Cloisonnement

- Une connexion MCP par **(entrée, serveur, version, membre)** dès que le
  serveur porte un patron d'identifiant. Deux personnes ne partagent jamais
  une connexion authentifiée avec le jeton de l'une d'elles.
- Un serveur sans identifiant n'a rien de personnel à isoler : sa connexion
  est partagée par l'organisation, un processus au lieu d'un par membre.
- Les connexions inutilisées sont fermées au bout de dix minutes, et seize
  au plus coexistent : cent membres actifs ne font pas cent processus
  éternels.
- Le domaine de permission est celui du plugin, `subagents` : une écriture
  exige `subagents.<portée>.write`, quelle que soit l'entrée. Les entrées
  d'un même catalogue ne se distinguent pas par leurs permissions.
- Le réglage du membre ne contient que des noms d'entrées. Ses identifiants
  vivent dans le magasin de secrets de l'hôte, scellés au repos.

## Le sous-agent netprobe

Le catalogue embarqué propose `netprobe`, adossé à
[netprobe-mcp](https://github.com/Bornholm/netprobe-mcp) : sonder un hôte,
un port, un site, un certificat, et diagnostiquer DNS, TLS et HTTP. Sept
outils, tous des lectures — exiger un « confirmer » pour chaque ping
rendrait le sous-agent inutilisable, et ce n'est pas là qu'est la barrière.

**La barrière est la liste d'autorisation de netprobe.** Il sonde depuis le
serveur : sans elle, c'est un SSRF offert sur le réseau interne. La
politique interne du serveur s'applique par défaut, et c'est celle qu'on
veut — l'Internet public autorisé, la boucle locale, les plages privées, le
lien-local et le point d'accès de métadonnées des fournisseurs cloud
(`169.254.169.254`, qui sert des identifiants d'instance) refusés.

Un exploitant qui veut sonder son propre réseau interne le décide en
fournissant son catalogue avec sa propre politique. C'est un geste
explicite, pas un défaut permissif.

> En 0.4.0, une politique explicite désactive les sondes DNS et TLS : leur
> section `probes:` fait paniquer le serveur au démarrage (étiquette YAML
> invalide dans son propre type de configuration). À revoir quand le
> correctif sera publié.

## Recette

La recette complète de l'entrée netprobe — installation vérifiée depuis la
release, connexion, découverte des outils, une sonde publique qui répond,
une cible du réseau interne refusée — est un test du dépôt. Il télécharge
et sonde Internet, il ne tourne donc pas en intégration continue :

```
cd plugins/subagents && SUBAGENTS_E2E=1 go test -run Netprobe -v ./...
```

Puis, en conversation : « est-ce que example.com répond en HTTPS ? » doit
passer par `delegate_to_netprobe`, et une cible en 192.168.0.0/16 doit être
refusée par la politique — pas par une erreur réseau.
