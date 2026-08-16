# Déploiement Dokku.
#
#   make dokku-setup     — préparation, une seule fois, de l'application distante
#   make dokku-storage   — création et propriétaire des volumes (accès root requis)
#   make dokku-env       — pousser les variables du fichier d'environnement local
#   make dokku-config    — pousser config.yaml vers le volume /config
#   make dokku-deploy    — déployer le HEAD courant
#   make dokku-scale     — démarrer le worker (après le premier déploiement)
#   make dokku-logs      — suivre les journaux
#   make dokku-qr        — retrouver le QR code de liaison WhatsApp
#   make dokku-build     — construire l'image exactement comme Dokku le fera
#
# Automata n'est pas un service web : pas de domaine, pas de port publié, pas
# de certificat. Il tourne en process "worker" (voir misc/dokku/Procfile), et
# Dokku ne lui route aucun trafic.

DOKKU_APP        ?= automata
DOKKU_HOST       ?= dokku.example.org
DOKKU_DEPLOY_URL ?= dokku@$(DOKKU_HOST)
# Certaines opérations sortent du périmètre du compte dokku, qui n'accepte que
# des commandes dokku : créer un sous-répertoire de volume et lui donner un
# propriétaire demande un accès administrateur distinct.
DOKKU_SSH_ADMIN  ?= root@$(DOKKU_HOST)
# Chemin du volume persistant côté hôte, tel que le crée `dokku storage:ensure-directory`.
DOKKU_STORAGE    ?= /var/lib/dokku/data/storage/$(DOKKU_APP)
# UID de l'utilisateur nonroot de l'image distroless : c'est lui qui doit
# posséder les volumes, sinon le premier démarrage échoue sur une écriture
# refusée.
DOKKU_UID        ?= 65532
# Fichier d'environnement local, tel que le produit `automata config init`.
DOKKU_ENV_FILE   ?= config/config.env
# Configuration locale déposée sur le volume /config.
DOKKU_CONFIG_FILE ?= config/config.yaml

DOKKU := ssh $(DOKKU_DEPLOY_URL)

.PHONY: dokku-setup dokku-storage dokku-env dokku-config dokku-deploy \
        dokku-scale dokku-logs dokku-qr dokku-healthcheck dokku-ps \
        dokku-build

# Préparation initiale de l'application. Idempotent : peut être relancé.
dokku-setup:
	-$(DOKKU) apps:create $(DOKKU_APP)
	# Le Dockerfile est à la racine du commit poussé (voir dokku-deploy). On
	# efface un éventuel chemin hérité : une application réutilisée peut en
	# porter un qui n'existe plus ici.
	$(DOKKU) builder-dockerfile:set $(DOKKU_APP) dockerfile-path
	# --chown false : le propriétaire est fixé par `make dokku-storage`, aucune
	# des valeurs proposées par Dokku ne correspondant à l'UID 65532 de
	# l'image distroless.
	$(DOKKU) storage:ensure-directory --chown false $(DOKKU_APP)
	-$(DOKKU) storage:mount $(DOKKU_APP) $(DOKKU_STORAGE)/data:/data
	-$(DOKKU) storage:mount $(DOKKU_APP) $(DOKKU_STORAGE)/config:/config
	@echo
	@echo "Montages en place — il doit y en avoir exactement deux, /data et /config :"
	@$(DOKKU) storage:list $(DOKKU_APP)
	@echo
	@echo "Ensuite :"
	@echo "  make dokku-storage    # créer les répertoires et leur propriétaire"
	@echo "  make dokku-env        # pousser les secrets"
	@echo "  make dokku-config     # déposer config.yaml"
	@echo "  make dokku-deploy     # premier déploiement"
	@echo "  make dokku-scale      # démarrer le worker"
	@echo "  make dokku-qr         # scanner le QR code de liaison WhatsApp"

# Crée les sous-répertoires du volume et leur donne le bon propriétaire.
#
# courier/ doit exister avant le premier démarrage : go-courier y écrit la
# session WhatsApp et ne crée pas l'arborescence lui-même.
dokku-storage:
	@echo "Création des répertoires sous $(DOKKU_STORAGE) (propriétaire $(DOKKU_UID))..."
	ssh $(DOKKU_SSH_ADMIN) "mkdir -p $(DOKKU_STORAGE)/data/courier $(DOKKU_STORAGE)/config && chown -R $(DOKKU_UID):$(DOKKU_UID) $(DOKKU_STORAGE) && ls -la $(DOKKU_STORAGE)"

# Pousse vers Dokku les variables du fichier d'environnement local.
#
# Les valeurs vides sont ignorées, pour ne pas écraser par du vide une clé
# déjà en place. Seuls les NOMS sont affichés : ce fichier contient des
# secrets.
dokku-env:
	@test -f $(DOKKU_ENV_FILE) || { echo "Aucun fichier $(DOKKU_ENV_FILE) — lancez 'automata config init' et renseignez-le."; exit 1; }
	@set -a; . ./$(DOKKU_ENV_FILE); set +a; \
	args=""; \
	for key in $$(grep -oE '^[A-Za-z_][A-Za-z0-9_]*=' $(DOKKU_ENV_FILE) | tr -d '='); do \
		value=$$(eval echo \$$$$key); \
		if [ -n "$$value" ]; then args="$$args $$key=$$value"; fi; \
	done; \
	if [ -z "$$args" ]; then echo "Aucune variable renseignée dans $(DOKKU_ENV_FILE)."; exit 1; fi; \
	echo "Variables poussées :"; for a in $$args; do echo "  $${a%%=*}"; done; \
	$(DOKKU) config:set --no-restart $(DOKKU_APP) $$args

# Dépose la configuration sur le volume /config.
#
# config.yaml n'est pas versionné et ne contient aucun secret : il décrit un
# déploiement, les valeurs sensibles restant des références d'environnement.
dokku-config:
	@test -f $(DOKKU_CONFIG_FILE) || { echo "Aucun fichier $(DOKKU_CONFIG_FILE) — lancez 'automata config init'."; exit 1; }
	scp $(DOKKU_CONFIG_FILE) $(DOKKU_SSH_ADMIN):$(DOKKU_STORAGE)/config/config.yaml
	ssh $(DOKKU_SSH_ADMIN) "chown $(DOKKU_UID):$(DOKKU_UID) $(DOKKU_STORAGE)/config/config.yaml"

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

# Démarre le worker. À lancer après le premier déploiement, Dokku ne
# connaissant les process qu'une fois l'image construite.
#
# Ne jamais dépasser 1 : la persistance est mono-écrivain et le scheduler
# s'appuie sur des verrous en mémoire, propres au processus.
dokku-scale:
	$(DOKKU) ps:scale $(DOKKU_APP) worker=1

dokku-ps:
	$(DOKKU) ps:report $(DOKKU_APP)

dokku-logs:
	$(DOKKU) logs $(DOKKU_APP) --tail

# Le QR code de liaison WhatsApp n'apparaît que dans les journaux, au premier
# démarrage, et expire vite. `ps:restart` en produit un nouveau.
dokku-qr:
	$(DOKKU) logs $(DOKKU_APP) --num 200

dokku-healthcheck:
	$(DOKKU) run $(DOKKU_APP) healthcheck

# Construit l'image exactement comme Dokku le fera, pour valider le Dockerfile
# avant de pousser. Vendorise puis nettoie, comme dokku-deploy.
dokku-build:
	@set -eu; \
	trap 'rm -rf vendor' EXIT; \
	go mod vendor; \
	docker build -f misc/dokku/Dockerfile -t $(DOKKU_APP):dokku .
