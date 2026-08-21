# Déploiement Dokku

Automata se déploie en **une image, un processus** : le worker de
messagerie et le serveur web (administration, pages de profil, webhook
Stripe, retours OAuth) vivent dans le même binaire et partagent une base
SQLite mono-écrivain. L'application est donc déclarée en `web`, avec un
domaine et un certificat — Stripe refuse d'appeler un webhook en clair, et
Google refuse une URL de redirection OAuth qui ne soit pas en HTTPS.

L'image embarque aussi les **services annexes** dont l'agent dépend :
SearXNG et son serveur MCP, en boucle locale du conteneur. Aucun d'eux
n'est exposé, et aucun n'a d'autre client qu'Automata.

## Préparer la configuration

```bash
automata config init                 # écrit config/config.yaml et config/config.env
```

L'entretien demande, entre autres, d'activer l'interface web (répondre
oui : elle porte l'administration, les profils, le webhook Stripe et les
retours OAuth) et les plugins. Gardez les valeurs par défaut proposées
pour un déploiement Dokku : données dans `/data`, plugins dans
`/plugins`, écoute sur `0.0.0.0:5000`.

Renseignez ensuite `config/config.env`. Trois valeurs se fabriquent :

```bash
openssl rand -base64 48 | tr -d '\n'   # WEB_SESSION_SECRET
openssl rand -base64 48 | tr -d '\n'   # STORAGE_ENCRYPTION_KEY  (à sauvegarder À PART)
automata web hash-password              # WEB_ADMIN_PASSWORD_HASH
```

`WEB_BASE_URL` est l'URL publique HTTPS de l'instance
(`https://automata.exemple.fr`) : elle compose les liens de profil et sert
d'adresse de retour aux paiements comme aux autorisations OAuth.

Vérifiez avant de pousser quoi que ce soit :

```bash
set -a; . config/config.env; set +a
automata config validate -config config/config.yaml
```

## Préparation, dans l'ordre

```bash
make dokku-setup      # créer l'app, volumes, domaine, port
make dokku-storage    # créer les répertoires et leur propriétaire (accès admin)
make dokku-env        # pousser les secrets
make dokku-config     # déposer config.yaml
make dokku-deploy     # premier déploiement
make dokku-scale      # démarrer l'application
make dokku-tls        # activer HTTPS
make dokku-qr         # scanner le QR code de liaison WhatsApp
```

Ensuite, `make dokku-deploy` suffit pour chaque mise à jour.

| Variable | Défaut | Rôle |
|---|---|---|
| `DOKKU_APP` | `automata` | Nom de l'application |
| `DOKKU_HOST` | `dokku.example.org` | Hôte Dokku |
| `DOKKU_DOMAIN` | `$(DOKKU_APP).$(DOKKU_HOST)` | Domaine public |
| `DOKKU_APP_PORT` | `5000` | Port écouté dans le conteneur |
| `DOKKU_SSH_ADMIN` | `root@$(DOKKU_HOST)` | Opérations hors périmètre dokku |
| `DOKKU_STORAGE` | `/var/lib/dokku/data/storage/$(DOKKU_APP)` | Volume persistant |
| `DOKKU_UID` | `977` | Propriétaire des volumes (utilisateur `searxng` de l'image) |
| `DOKKU_ENV_FILE` | `config/config.env` | Fichier lu par `dokku-env` |
| `DOKKU_CONFIG_FILE` | `config/config.yaml` | Configuration déposée sur `/config` |

## Ce que le config.yaml de déploiement doit dire

```yaml
storage:
  encryption_key: ${STORAGE_ENCRYPTION_KEY}
  application:
    path: /data/app.sqlite
web:
  enabled: true
  addr: 0.0.0.0:5000        # 0.0.0.0 IMPÉRATIF, voir ci-dessous
  base_url: https://automata.exemple.fr
plugins:
  enabled: true
  dir: /plugins             # binaires construits par l'image
mcp_servers:
  internet-search:
    transport: streamable-http
    url: ${INTERNET_SEARCH_MCP_URL}      # renseignée par le conteneur
    headers:
      Authorization: Bearer ${INTERNET_SEARCH_MCP_TOKEN}
```

Les chemins pointent les volumes montés : `/data` (bases SQLite, index de
mémoire, session WhatsApp) et `/config`. Les prompts et les plugins, eux,
font partie de l'image — c'est du code, il se déploie avec l'application.

**`STORAGE_ENCRYPTION_KEY` se sauvegarde à part.** La perdre rend
illisibles les conversations et les souvenirs déjà chiffrés, sauvegardes
comprises.

## Services annexes, et comment en ajouter

Le point d'entrée de l'image (`entrypoint.sh`) ne connaît aucun service en
particulier : il charge tout ce qu'il trouve dans
`/etc/automata/services.d/*.sh`, dans l'ordre alphabétique, puis lance
Automata. Ajouter un serveur MCP à l'image, c'est donc **ajouter un
fichier** dans `misc/dokku/services.d/`, jamais modifier le superviseur.

Un fichier de service est *sourcé*, et définit :

```sh
service_start() {          # obligatoire — lance et publie son PID
    monserveur --port 4000 &
    service_pid=$!
    export MON_MCP_URL=http://127.0.0.1:4000/mcp   # lu par config.yaml
}

service_ready() {          # facultatif — sondé jusqu'à 60 s
    node -e "fetch('http://127.0.0.1:4000/health').then(()=>process.exit(0),()=>process.exit(1))"
}
```

Deux propriétés en découlent, dont dépend la fiabilité du déploiement :

- **les variables exportées atteignent Automata**, qui est lancé après les
  services : un jeton engendré au démarrage n'a donc jamais besoin d'être
  configuré ni stocké (voir `10-searxng.sh`) ;
- **les destins sont liés** : si un service meurt, le conteneur s'arrête.
  Sans cela, Dokku verrait une application saine alors que la recherche,
  par exemple, ne répondrait plus.

Un service qui renonce (retour non-zéro) arrête le démarrage : mieux vaut
un conteneur qui refuse de partir qu'un agent amputé d'une capacité qu'il
croit avoir.

Le binaire du serveur MCP doit évidemment être présent dans l'image :
ajoutez son étage de construction et sa copie dans le `Dockerfile`, sur le
modèle de `mcp-searxng`.

## Éprouver avant de pousser

```bash
make dokku-build     # construire l'image
make dokku-run       # la lancer, mêmes volumes, même configuration
```

`dokku-run` monte `local/dokku-data` et `local/dokku-config` : y déposer
un `config.yaml` aux chemins du conteneur suffit à valider une image
complète — services annexes, plugins et serveur web compris — sans rien
déployer. Les volumes locaux doivent appartenir à l'utilisateur qui lance
le conteneur.

## Si la sonde de démarrage échoue

**Cause la plus fréquente : l'ancien conteneur tourne encore.** Dokku
déploie sans coupure — nouveau conteneur, healthchecks, puis arrêt de
l'ancien. Les deux montent le même `/data`, et la persistance d'Automata
est à écrivain unique. `make dokku-deploy` arrête donc l'application
avant le push ; si vous poussez à la main, faites-le vous-même :

```bash
dokku ps:stop automata && git push dokku …
```

Depuis le verrou d'instance, le symptôme est nommé au lieu d'être muet :

```
registry: une autre instance d'Automata utilise déjà "/data" (verrou /data/.automata.lock)
```

Sans ce verrou, le second processus s'arrêtait sans un mot sur le verrou
bolt de l'index bleve, et le seul signe visible était un démarrage qui ne
finissait jamais.

## Si la sonde échoue toujours

`Failure in name='administration joignable': dial tcp …:5000: connect:
connection refused` a presque toujours la même cause : **`web.addr`
écoute en boucle locale**. Dans un conteneur, `127.0.0.1:5000` n'est
joignable par personne — ni le proxy, ni la sonde. Il faut
`0.0.0.0:5000`. Depuis, Automata le signale lui-même au démarrage :

```
web: écoute en boucle locale dans un conteneur, injoignable depuis l'extérieur
```

Vérifiez la configuration effectivement déposée sur le volume :

```bash
ssh root@<hôte> "grep -A3 '^web:' /var/lib/dokku/data/storage/automata/config/config.yaml"
```

Si l'adresse est bonne, lisez les jalons de démarrage : chaque étape est
journalisée avec sa durée, et la dernière franchie désigne celle qui
bloque.

```bash
dokku logs automata | grep "étape de démarrage"
```

## Un piège d'assemblage à connaître

L'image ne déclare **aucun ENTRYPOINT** (`ENTRYPOINT []`), et c'est
délibéré. Dokku exécute la commande du `Procfile` : tout ENTRYPOINT la
transformerait en simples *arguments*, et Automata recevrait son propre
chemin en première position — il refuserait de démarrer sur « le drapeau
-config est requis ». Sans la remise à zéro explicite, c'est l'ENTRYPOINT
hérité de l'image SearXNG qui prendrait la main, et granian démarrerait à
la place du superviseur.

Le superviseur est donc la **commande** du conteneur, jamais son
entrypoint — dans le `Procfile` comme dans le `CMD` par défaut.

## Journaux et diagnostic

```bash
make dokku-logs         # suivre
make dokku-ps           # état des process
make dokku-healthcheck  # sonde interne
make dokku-qr           # 200 dernières lignes (QR WhatsApp)
```

Le QR code de liaison WhatsApp n'apparaît qu'au premier démarrage et
expire vite ; `dokku ps:restart` en produit un nouveau.
