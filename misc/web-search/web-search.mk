# Conteneur de recherche web (SearXNG + serveur MCP).
#
#   make web-search-build  — construire l'image
#   make web-search-up     — démarrer le conteneur (publié sur la boucle locale)
#   make web-search-down    — arrêter et supprimer le conteneur
#   make web-search-logs   — suivre les journaux
#   make web-search-test   — vérifier la chaîne complète depuis automata
#
# Le jeton est lu dans local/web-search.token, créé au besoin par
# web-search-up. Voir misc/web-search/README.md.

WEB_SEARCH_IMAGE ?= automata-web-search:latest
WEB_SEARCH_NAME  ?= automata-web-search
# Publication sur la boucle locale seulement : ce point d'entrée exécute des
# recherches sortantes pour quiconque l'atteint avec le jeton.
WEB_SEARCH_BIND  ?= 127.0.0.1
WEB_SEARCH_PORT  ?= 3000
WEB_SEARCH_TOKEN_FILE ?= local/web-search.token
WEB_SEARCH_URL   ?= http://$(WEB_SEARCH_BIND):$(WEB_SEARCH_PORT)/mcp

.PHONY: web-search-build web-search-up web-search-down web-search-logs web-search-test web-search-token

web-search-build:
	docker build -t $(WEB_SEARCH_IMAGE) misc/web-search

$(WEB_SEARCH_TOKEN_FILE):
	@mkdir -p $(dir $@)
	@head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' > $@
	@chmod 600 $@
	@echo "jeton MCP écrit dans $@"

web-search-token: $(WEB_SEARCH_TOKEN_FILE)

web-search-up: web-search-build $(WEB_SEARCH_TOKEN_FILE)
	docker rm -f $(WEB_SEARCH_NAME) 2>/dev/null || true
	docker run -d --name $(WEB_SEARCH_NAME) --restart unless-stopped \
		-p $(WEB_SEARCH_BIND):$(WEB_SEARCH_PORT):3000 \
		-e MCP_HTTP_AUTH_TOKEN=$$(cat $(WEB_SEARCH_TOKEN_FILE)) \
		$(WEB_SEARCH_IMAGE)
	@echo "recherche web disponible sur $(WEB_SEARCH_URL)"

web-search-down:
	docker rm -f $(WEB_SEARCH_NAME) 2>/dev/null || true

web-search-logs:
	docker logs -f $(WEB_SEARCH_NAME)

web-search-test: $(WEB_SEARCH_TOKEN_FILE)
	AUTOMATA_WEB_SEARCH_URL=$(WEB_SEARCH_URL) \
	AUTOMATA_WEB_SEARCH_TOKEN=$$(cat $(WEB_SEARCH_TOKEN_FILE)) \
	go test ./internal/mcp/ -run WebSearchContainer -v -count=1
