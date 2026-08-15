module github.com/bornholm/automata

go 1.26.5

require (
	github.com/bornholm/go-courier v0.0.0-20250902080206-c967c407f1e1
	github.com/dustin/go-humanize v1.0.1
	github.com/ncruces/go-sqlite3 v0.35.2
	github.com/robfig/cron/v3 v3.0.1
	github.com/spf13/cobra v1.10.2
	gopkg.in/yaml.v3 v3.0.1
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mdp/qrterminal v1.0.1 // indirect
	github.com/ncruces/go-sqlite3-wasm/v3 v3.2.35303 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/petermattis/goid v0.0.0-20250813065127-a731cc31b4fe // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/rs/zerolog v1.34.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
	go.mau.fi/libsignal v0.2.0 // indirect
	go.mau.fi/util v0.9.0 // indirect
	go.mau.fi/whatsmeow v0.0.0-20250829123043-72d2ed58e998 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/exp v0.0.0-20250813145105-42675adae3e6 // indirect
	golang.org/x/net v0.55.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	google.golang.org/protobuf v1.36.8 // indirect
	rsc.io/qr v0.2.0 // indirect
)

// go-courier n'a pas encore publié de version taguée pour l'API réellement
// utilisée ici (le module fetché par le proxy correspond au seul commit
// public, obsolète par rapport au dépôt de travail local). On pointe donc
// vers le dépôt local, dont le code source fait foi (voir
// docs/integration-inventory.md §1 et AGENTS.md).
replace github.com/bornholm/go-courier => /home/wpetit/workspace/go-courier
