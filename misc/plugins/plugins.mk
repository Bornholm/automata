# Outillage du système de plugins : génération du proto gRPC et compilation
# des plugins embarqués vers le répertoire de découverte local.

tools/protoc-gen-go/bin/protoc-gen-go:
	mkdir -p tools/protoc-gen-go/bin
	GOBIN=$(PWD)/tools/protoc-gen-go/bin go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

tools/protoc-gen-go-grpc/bin/protoc-gen-go-grpc:
	mkdir -p tools/protoc-gen-go-grpc/bin
	GOBIN=$(PWD)/tools/protoc-gen-go-grpc/bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Les .pb.go sont commités : go build reste autonome, protoc n'est requis
# que pour faire évoluer le contrat.
generate-proto: tools/protoc-gen-go/bin/protoc-gen-go tools/protoc-gen-go-grpc/bin/protoc-gen-go-grpc
	PATH=$(PWD)/tools/protoc-gen-go/bin:$(PWD)/tools/protoc-gen-go-grpc/bin:$(PATH) \
	protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		pkg/pluginsdk/proto/plugin.proto

# Compile chaque plugin embarqué vers local/plugins/ (le répertoire déclaré
# par plugins.dir de la configuration locale).
build-plugins:
	@for d in plugins/*/ ; do \
		name=$$(basename $$d) ; \
		echo "plugin $$name" ; \
		(cd $$d && go build -o ../../local/plugins/$$name .) || exit 1 ; \
	done
