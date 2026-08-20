.PHONY: fmt build test vet race watch watch-build check-config run-with-env

fmt:
	gofmt -l -w .

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

race:
	go test -race ./...

# Fichiers locaux, non versionnés, produits par `automata config init`.
CONFIG_FILE ?= config/config.yaml
CONFIG_ENV  ?= config/config.env
# Exportés : modd.conf ne reçoit pas les variables de make autrement, et son
# daemon lancerait la configuration par défaut au lieu de celle demandée.
export CONFIG_FILE
export CONFIG_ENV

# Développement à chaud : modd surveille les sources, la configuration locale et
# ses secrets, recompile puis relance le worker.
watch: check-config tools/modd/bin/modd
	rm -f watch.log
	tools/modd/bin/modd

watch-build:
	go build -o bin/automata ./cmd/automata

# L'entretien de `config init` est interactif : on ne peut pas le déclencher
# depuis une règle. On se contente donc de dire ce qui manque.
check-config:
	@test -f $(CONFIG_FILE) || { \
		echo "$(CONFIG_FILE) est absent. Deux façons de le créer :"; \
		echo "  go run ./cmd/automata config init -output $(CONFIG_FILE)   # entretien guidé"; \
		echo "  cp config/config.example.yaml $(CONFIG_FILE)               # à adapter à la main"; \
		exit 1; }
	@test -f $(CONFIG_ENV) || { \
		echo "$(CONFIG_ENV) est absent : il porte les secrets référencés par $${VARIABLE}."; \
		echo "Il est produit par la même commande que $(CONFIG_FILE)."; \
		exit 1; }

# Exécute une commande avec les secrets locaux exportés : ce sont les valeurs
# que les références $${VARIABLE} de la configuration vont chercher dans
# l'environnement.
run-with-env: check-config
	( set -o allexport && . ./$(CONFIG_ENV) && set +o allexport && $(value CMD) )

tools/modd/bin/modd:
	mkdir -p tools/modd/bin
	GOBIN=$(PWD)/tools/modd/bin go install github.com/cortesi/modd/cmd/modd@latest

include misc/**/*.mk
