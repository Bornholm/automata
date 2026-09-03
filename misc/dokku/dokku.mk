# Déploiement Dokku.
#
#   make dokku-setup     — préparation, une seule fois, de l'application distante
#   make dokku-storage   — création et propriétaire des volumes (accès root requis)
#   make dokku-env       — pousser les variables du fichier d'environnement local
#   make dokku-config    — valider puis pousser config.yaml vers /config
#   make dokku-deploy    — déployer le HEAD courant
#   make dokku-scale     — démarrer l'application (après le premier déploiement)
#   make dokku-tls       — activer HTTPS (Let's Encrypt)
#   make dokku-logs      — suivre les journaux
#   make dokku-qr        — retrouver le QR code de liaison WhatsApp
#   make dokku-healthcheck — interroger GET /healthz par l'URL publique
#   make dokku-build     — construire l'image exactement comme Dokku le fera
#   make dokku-run       — lancer l'image en local, comme en production
#
# Automata est un service web depuis le socle SaaS : administration,
# pages de profil, webhook Stripe et retours OAuth. Il est donc déclaré en
# process "web" (voir misc/dokku/Procfile), avec un domaine et un
# certificat — Stripe comme Google exigent une URL publique en HTTPS.
#
# Un seul processus, jamais deux : le worker de messagerie et le serveur
# web vivent dans le même binaire et partagent une base SQLite
# mono-écrivain.

DOKKU_APP        ?= automata
DOKKU_HOST       ?= dokku.example.org
DOKKU_DEPLOY_URL ?= dokku@$(DOKKU_HOST)
# Certaines opérations sortent du périmètre du compte dokku, qui n'accepte que
# des commandes dokku : créer un sous-répertoire de volume et lui donner un
# propriétaire demande un accès administrateur distinct.
DOKKU_SSH_ADMIN  ?= root@$(DOKKU_HOST)
# Chemin du volume persistant côté hôte, tel que le crée `dokku storage:ensure-directory`.
DOKKU_STORAGE    ?= /var/lib/dokku/data/storage/$(DOKKU_APP)
# UID de l'utilisateur searxng de l'image finale : c'est lui qui doit
# posséder les volumes, sinon le premier démarrage échoue sur une écriture
# refusée.
DOKKU_UID        ?= 977
# Fichier d'environnement local, tel que le produit `automata config init`.
DOKKU_ENV_FILE   ?= config/config.env
# Configuration locale déposée sur le volume /config.
DOKKU_CONFIG_FILE ?= config/config.yaml
# Domaine public de l'application. Le webhook Stripe et le retour OAuth de
# Google doivent l'atteindre : ce ne peut pas être une adresse privée.
DOKKU_DOMAIN     ?= $(DOKKU_APP).$(DOKKU_HOST)
# Port écouté par le serveur web dans le conteneur, tel que déclaré par
# web.addr de config.yaml (0.0.0.0:5000).
DOKKU_APP_PORT   ?= 5000
# Adresse de contact du certificat Let's Encrypt.
DOKKU_LETSENCRYPT_EMAIL ?= $(shell git config user.email)

DOKKU := ssh $(DOKKU_DEPLOY_URL)

.PHONY: dokku-setup dokku-storage dokku-env dokku-config dokku-config-check dokku-deploy \
        dokku-scale dokku-tls dokku-logs dokku-qr dokku-healthcheck dokku-ps \
        dokku-build dokku-run dokku-healthcheck-local

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
	# Le trafic public entre par le proxy vers le port du serveur web.
	-$(DOKKU) domains:add $(DOKKU_APP) $(DOKKU_DOMAIN)
	$(DOKKU) ports:set $(DOKKU_APP) http:80:$(DOKKU_APP_PORT)
	# Les pièces jointes (photos, notes vocales) transitent par les pages
	# de profil : la valeur par défaut du proxy est trop basse.
	$(DOKKU) nginx:set $(DOKKU_APP) client-max-body-size 25m
	@echo
	@echo "Ensuite :"
	@echo "  make dokku-storage    # créer les répertoires et leur propriétaire"
	@echo "  make dokku-env        # pousser les secrets"
	@echo "  make dokku-config     # déposer config.yaml"
	@echo "  make dokku-deploy     # premier déploiement"
	@echo "  make dokku-scale      # démarrer l'application"
	@echo "  make dokku-tls        # activer HTTPS (exigé par Stripe et Google)"
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
#
# Les valeurs sont lues LITTÉRALEMENT et transmises en arguments quotés.
# Deux réexpansions guettaient, et la seconde a déjà détruit une valeur en
# production : un hash bcrypt commence par « $2a$10$ », dont le shell fait
# des paramètres positionnels vides.
#
#  1. côté local : ne jamais faire « eval echo $VALEUR » ni sourcer le
#     fichier pour relire les variables — la valeur doit sortir du fichier
#     telle quelle ;
#  2. côté distant : ssh concatène ses arguments et les fait interpréter
#     par un shell. Chaque valeur part donc entre guillemets SIMPLES, les
#     apostrophes internes échappées.
#
# Symptôme si la protection saute : l'authentification de l'administration
# échoue sans message clair, le hash stocké ayant perdu son préfixe.
dokku-env:
	@test -f $(DOKKU_ENV_FILE) || { echo "Aucun fichier $(DOKKU_ENV_FILE) — lancez 'automata config init' et renseignez-le."; exit 1; }
	@set --; \
	while IFS= read -r line; do \
		case "$$line" in ''|\#*) continue;; esac; \
		key=$${line%%=*}; \
		case "$$key" in *[!A-Za-z0-9_]*) continue;; esac; \
		value=$${line#*=}; \
		[ -n "$$value" ] || continue; \
		esc=$$(printf '%s' "$$value" | sed "s/'/'\\\\''/g"); \
		set -- "$$@" "$$key='$$esc'"; \
	done < $(DOKKU_ENV_FILE); \
	if [ "$$#" -eq 0 ]; then echo "Aucune variable renseignée dans $(DOKKU_ENV_FILE)."; exit 1; fi; \
	echo "Variables poussées :"; for a in "$$@"; do echo "  $${a%%=*}"; done; \
	$(DOKKU) config:set --no-restart $(DOKKU_APP) "$$@"

# Dépose la configuration sur le volume /config.
#
# config.yaml n'est pas versionné et ne contient aucun secret : il décrit un
# déploiement, les valeurs sensibles restant des références d'environnement.
# Une configuration refusée au démarrage ne se voit pas : Dokku relance le
# conteneur en boucle, et l'assistant reste muet sans qu'aucune alerte ne
# parte. Elle est donc validée ici, avec les variables réellement poussées,
# AVANT de partir. Seul le jeton du serveur MCP intégré reçoit une valeur de
# façade : il est engendré à chaque démarrage par l'entrypoint du conteneur,
# et n'existe donc dans aucun fichier.
dokku-config: dokku-config-check
	scp $(DOKKU_CONFIG_FILE) $(DOKKU_SSH_ADMIN):$(DOKKU_STORAGE)/config/config.yaml
	ssh $(DOKKU_SSH_ADMIN) "chown $(DOKKU_UID):$(DOKKU_UID) $(DOKKU_STORAGE)/config/config.yaml"

dokku-config-check:
	@test -f $(DOKKU_CONFIG_FILE) || { echo "Aucun fichier $(DOKKU_CONFIG_FILE) — lancez 'automata config init'."; exit 1; }
	@test -f $(DOKKU_ENV_FILE) || { echo "Aucun fichier $(DOKKU_ENV_FILE) — lancez 'automata config init' et renseignez-le."; exit 1; }
	@set -a; . ./$(DOKKU_ENV_FILE); set +a; \
	export INTERNET_SEARCH_MCP_TOKEN=$${INTERNET_SEARCH_MCP_TOKEN:-jeton-engendre-au-demarrage}; \
	go run ./cmd/automata config validate --config $(DOKKU_CONFIG_FILE)

# Déploie le commit courant.
#
# Dokku construit l'image sur son serveur à partir du seul dépôt poussé.
# Cela fonctionne sans artifice depuis que go-courier, genai et amoxtli
# sont publiés sur le module proxy : la seule directive "replace" restante
# (pkg/pluginsdk) pointe à l'intérieur du dépôt, donc le commit poussé
# l'emporte. La vendorisation de 85 Mio que cette cible pratiquait
# auparavant n'a plus lieu d'être.
#
# Le Dockerfile et le Procfile vivent sous misc/dokku/ ; ils sont hissés à
# la racine dans un commit fabriqué pour l'occasion, dans un index
# temporaire. Rien n'est modifié dans le dépôt de travail, pas même
# l'index.
#
# --force parce que Dokku n'a pas d'historique commun avec le dépôt local
# dès que l'on a rebasé.
#
# L'application est ARRÊTÉE avant le push. Dokku déploie sans coupure :
# il démarre le nouveau conteneur, attend ses healthchecks, puis arrête
# l'ancien. Or les deux montent le même volume /data, et la persistance
# d'Automata est mono-écrivain (verrou d'instance, verrou bolt de
# l'index bleve) : le nouveau conteneur serait refusé par le verrou, ses
# healthchecks échoueraient, et le déploiement serait rejeté sans que
# rien n'ait changé. Une courte interruption est le prix d'un volume à
# écrivain unique — c'est la même raison qui interdit ps:scale au-delà
# de 1.
dokku-deploy:
	@set -eu; \
	echo "Arrêt de l'application (volume à écrivain unique)..."; \
	$(DOKKU) ps:stop $(DOKKU_APP) >/dev/null 2>&1 || true; \
	if [ -n "$$(git status --porcelain)" ]; then \
		echo "Attention : modifications non commitées, seul HEAD sera déployé."; \
	fi; \
	tmp_index=$$(mktemp); \
	trap 'rm -f "$$tmp_index"' EXIT; \
	export GIT_INDEX_FILE="$$tmp_index"; \
	git read-tree HEAD; \
	git update-index --add --cacheinfo 100644,$$(git hash-object -w misc/dokku/Dockerfile),Dockerfile; \
	git update-index --add --cacheinfo 100644,$$(git hash-object -w misc/dokku/Procfile),Procfile; \
	git update-index --add --cacheinfo 100644,$$(git hash-object -w misc/dokku/app.json),app.json; \
	tree=$$(git write-tree); \
	commit=$$(git commit-tree "$$tree" -p HEAD -m "deploy: $$(git rev-parse --short HEAD)"); \
	echo "Déploiement de $$commit vers $(DOKKU_DEPLOY_URL):$(DOKKU_APP)..."; \
	git push --force "$(DOKKU_DEPLOY_URL):$(DOKKU_APP)" "$$commit:refs/heads/master"

# Démarre l'application. À lancer après le premier déploiement, Dokku ne
# connaissant les process qu'une fois l'image construite.
#
# Ne jamais dépasser 1 : la persistance est mono-écrivain, le scheduler
# s'appuie sur des verrous en mémoire, et deux conteneurs se disputeraient
# la session WhatsApp.
dokku-scale:
	$(DOKKU) ps:scale $(DOKKU_APP) web=1

# Certificat Let's Encrypt. Stripe refuse d'appeler un webhook en clair, et
# Google refuse une URL de redirection OAuth qui ne soit pas HTTPS (hors
# 127.0.0.1) : sans certificat, ni paiements ni connexion Gmail.
dokku-tls:
	-$(DOKKU) letsencrypt:set $(DOKKU_APP) email $(DOKKU_LETSENCRYPT_EMAIL)
	$(DOKKU) letsencrypt:enable $(DOKKU_APP)

dokku-ps:
	$(DOKKU) ps:report $(DOKKU_APP)

dokku-logs:
	$(DOKKU) logs $(DOKKU_APP) --tail

# Le QR code de liaison WhatsApp n'apparaît que dans les journaux, au premier
# démarrage, et expire vite. `ps:restart` en produit un nouveau.
dokku-qr:
	$(DOKKU) logs $(DOKKU_APP) --num 200

# Interroge la sonde du service (GET /healthz) par son URL publique : elle
# répond 200 quand la base est joignable et le câblage interne terminé.
# Passer par le domaine éprouve toute la chaîne — nginx, certificat,
# application — là où un appel depuis le conteneur ne dirait rien du proxy.
dokku-healthcheck:
	@set -eu; \
	url="https://$(DOKKU_DOMAIN)/healthz"; \
	echo "Sonde $$url"; \
	code=$$(curl -sS -o /tmp/automata-healthz -w '%{http_code}' --max-time 10 "$$url" || echo 000); \
	body=$$(cat /tmp/automata-healthz 2>/dev/null || true); \
	rm -f /tmp/automata-healthz; \
	if [ "$$code" = "200" ]; then \
		echo "OK ($$code) : $$body"; \
	else \
		echo "ÉCHEC ($$code) : $$body"; \
		echo "Diagnostic : make dokku-ps, puis make dokku-logs."; \
		exit 1; \
	fi

# La même sonde, mais DEPUIS le conteneur en cours : elle court-circuite
# nginx et le certificat, ce qui départage une application en panne d'un
# proxy mal configuré.
#
# `enter`, jamais `run` : `run` démarre un conteneur NEUF, où rien n'écoute
# sur le port du serveur web — la sonde y échouerait toujours, et sur une
# base montée par un second écrivain de surcroît. Le binaire est nommé par
# son chemin absolu : l'entrypoint de l'image est vide (voir le Dockerfile)
# et le PATH n'est pas garanti.
#
# Silencieuse quand tout va bien : seul le code de sortie compte (0 prêt,
# 1 sinon).
dokku-healthcheck-local:
	$(DOKKU) enter $(DOKKU_APP) web /usr/local/bin/automata healthcheck \
		-addr 127.0.0.1:$(DOKKU_APP_PORT) -path /healthz

# Construit l'image exactement comme Dokku le fera, pour valider le
# Dockerfile avant de pousser.
dokku-build:
	docker build -f misc/dokku/Dockerfile -t $(DOKKU_APP):dokku .

# Lance l'image en local, comme en production : mêmes volumes, même
# configuration, mêmes services annexes. C'est la façon d'éprouver un
# déploiement sans pousser quoi que ce soit.
dokku-run: dokku-build
	docker run --rm -it \
		-p $(DOKKU_APP_PORT):$(DOKKU_APP_PORT) \
		-v "$(PWD)/local/dokku-data:/data" \
		-v "$(PWD)/local/dokku-config:/config" \
		$$(test -f $(DOKKU_ENV_FILE) && echo --env-file $(DOKKU_ENV_FILE)) \
		$(DOKKU_APP):dokku
