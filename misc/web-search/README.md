# Recherche web : SearXNG et son serveur MCP dans un conteneur

Ce répertoire construit l'image qui donne à l'agent une capacité de recherche
web sans dépendre d'un service tiers : une instance [SearXNG] privée et le
serveur MCP [mcp-searxng] qui l'expose à l'agent, réunis dans un conteneur.

Les réunir n'est pas une commodité. Le serveur MCP est le seul client de
SearXNG : séparés, il faudrait exposer l'API JSON de SearXNG sur le réseau,
alors qu'ici elle ne quitte jamais la boucle locale du conteneur. Une seule
surface est publiée — le point d'entrée MCP — et elle exige un jeton.

## Démarrer

```sh
make web-search-up     # construit l'image et démarre le conteneur
make web-search-test   # vérifie la chaîne complète depuis automata
make web-search-logs   # suivre les journaux
make web-search-down   # arrêter
```

`web-search-up` génère au besoin un jeton dans `local/web-search.token`
(mode 600, répertoire déjà ignoré par git) et publie le point d'entrée sur
`127.0.0.1:3000` uniquement. Cette restriction compte : quiconque atteint ce
port avec le jeton fait exécuter des requêtes sortantes par le conteneur.

## Brancher sur l'agent

```yaml
mcp_servers:
  internet-search:
    transport: streamable-http
    url: http://127.0.0.1:3000/mcp
    headers:
      Authorization: Bearer ${WEB_SEARCH_MCP_TOKEN}
    permission_domain: search
```

`streamable-http`, pas `http` : mcp-searxng parle la révision 2025-03-26 du
protocole. Le transport `http` d'automata parle l'ancien HTTP+SSE, et les
deux ne s'entendent pas — un serveur streamable rejette le GET permanent
qu'ouvre le client SSE.

Aucune confirmation d'écriture n'est nécessaire : les quatre outils exposés
(`searxng_web_search`, `searxng_search_suggestions`, `searxng_instance_info`,
`web_url_read`) sont tous en lecture. Surveiller en revanche les limites de
l'agent qui les utilise (`agents.<nom>.limits.max_tool_result_bytes` et
`max_tool_context_bytes`) : une page entière rapportée par `web_url_read`
remplit vite la fenêtre de contexte.

## Réglages

Tout se règle par variables d'environnement du conteneur (`docker run -e`),
ou en modifiant `WEB_SEARCH_*` dans `web-search.mk`.

| Variable | Rôle |
| --- | --- |
| `MCP_HTTP_AUTH_TOKEN` | Jeton exigé sur chaque requête. **Requis** ; le conteneur refuse de démarrer sans lui. |
| `SEARXNG_SECRET` | Clé de session SearXNG. Générée à chaque démarrage si absente, ce qui suffit ici : aucun client MCP n'utilise de cookie de préférences. |
| `SEARXNG_DEFAULT_LANGUAGE` | Langue par défaut des recherches (`fr` dans l'image). |
| `SEARXNG_MAX_RESULTS` | Nombre de résultats renvoyés par recherche (10 dans l'image). |
| `SEARXNG_MAX_RESULT_CHARS` | Plafond de caractères par extrait de résultat (500 dans l'image). |
| `URL_READ_MAX_CHARS` | Plafond par défaut de `web_url_read` (8000 dans l'image, sous les 16 KiB de `max_tool_result_bytes`). Le modèle peut demander moins avec `maxLength`, jamais plus. |

Les réglages de SearXNG lui-même sont dans `searxng-settings.yml`, superposé
aux valeurs par défaut du projet amont. Le format `json` y est indispensable :
c'est la voie qu'emprunte le serveur MCP, et sans lui chaque recherche échoue
en 403.

Au démarrage, quelques moteurs peuvent échouer à s'initialiser (Wikidata
répond parfois 403 à une IP de sortie inconnue). SearXNG les écarte et
interroge les autres : ce n'est pas une panne, la recherche fonctionne.

## Mise à jour

La version du serveur MCP est épinglée dans le `Dockerfile`
(`MCP_SEARXNG_VERSION`), SearXNG suit `searxng/searxng:latest`. Pour monter
de version :

```sh
docker build --build-arg MCP_SEARXNG_VERSION=1.16.0 --pull -t automata-web-search:latest misc/web-search
make web-search-test
```

[SearXNG]: https://github.com/searxng/searxng
[mcp-searxng]: https://github.com/ihor-sokoliuk/mcp-searxng
