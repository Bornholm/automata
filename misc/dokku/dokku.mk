DOKKU_APP ?= automata
DOKKU_DEPLOY_URL ?= dokku@dokku.example.org

# dokku-deploy pousse HEAD vers Dokku, augmenté des dépendances vendorisées.
#
# Pourquoi ce détour plutôt qu'un simple `git push` comme ailleurs : go.mod
# redirige go-courier, genai et amoxtli vers des répertoires frères du dépôt
# (directives "replace"), faute de version publiée. Dokku construit l'image
# sur son serveur à partir du seul dépôt poussé, où ces répertoires n'existent
# pas : le build échouerait dès `go mod download`.
#
# `go mod vendor` copie ces dépendances dans vendor/, que le commit poussé
# emporte. Les 85 Mio produits n'entrent jamais dans l'historique local : le
# commit est fabriqué dans un index temporaire, poussé, puis abandonné. Rien
# n'est modifié dans le dépôt de travail, pas même l'index.
#
# Le jour où les trois bibliothèques publient une version taguée, cette cible
# redevient le `git push` d'une ligne des autres projets.
dokku-deploy:
	@set -eu; \
	if [ -n "$$(git status --porcelain)" ]; then \
		echo "Attention : modifications non commitées, seul HEAD sera déployé."; \
	fi; \
	echo "Vendorisation des dépendances..."; \
	go mod vendor; \
	tmp_index=$$(mktemp); \
	trap 'rm -f "$$tmp_index"; rm -rf vendor' EXIT; \
	export GIT_INDEX_FILE="$$tmp_index"; \
	git read-tree HEAD; \
	git add -f vendor; \
	git update-index --add --cacheinfo 100644,$$(git hash-object -w misc/dokku/Dockerfile),Dockerfile; \
	git update-index --add --cacheinfo 100644,$$(git hash-object -w misc/dokku/Procfile),Procfile; \
	tree=$$(git write-tree); \
	commit=$$(git commit-tree "$$tree" -p HEAD -m "deploy: $$(git rev-parse --short HEAD) with vendored dependencies"); \
	echo "Déploiement de $$commit vers $(DOKKU_DEPLOY_URL):$(DOKKU_APP)..."; \
	git push --force "$(DOKKU_DEPLOY_URL):$(DOKKU_APP)" "$$commit:refs/heads/master"

# dokku-logs suit les journaux de l'application.
dokku-logs:
	ssh $(DOKKU_DEPLOY_URL) logs $(DOKKU_APP) -t

# dokku-qr affiche les journaux récents, où se trouve le QR code de liaison
# WhatsApp au premier démarrage.
dokku-qr:
	ssh $(DOKKU_DEPLOY_URL) logs $(DOKKU_APP) --num 200

# dokku-shell ouvre un shell dans le conteneur. L'image finale étant une
# distroless sans shell, la commande ne sert qu'à lancer le binaire lui-même,
# par exemple la sonde de santé.
dokku-healthcheck:
	ssh $(DOKKU_DEPLOY_URL) run $(DOKKU_APP) healthcheck

.PHONY: dokku-deploy dokku-logs dokku-qr dokku-healthcheck
