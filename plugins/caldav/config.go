package main

import (
	"encoding/json"
	"fmt"

	"github.com/bornholm/automata/pkg/pluginsdk"
)

// memberConfig est la configuration par membre, rangée par l'hôte et
// scellée au repos. Aucun secret n'y figure : le mot de passe passe par
// SetSecret et n'est jamais relu vers l'interface.
type memberConfig struct {
	// ServerURL est le point d'entrée CalDAV (souvent une URL de
	// découverte : l'agenda précis est résolu ensuite).
	ServerURL string `json:"server_url"`
	Username  string `json:"username"`

	// CalendarPath est la collection choisie par le membre. Vide tant
	// qu'aucun agenda n'a été sélectionné : le premier trouvé sert alors
	// par défaut.
	CalendarPath string `json:"calendar_path,omitempty"`
	// CalendarName accompagne le chemin pour l'afficher sans réinterroger
	// le serveur.
	CalendarName string `json:"calendar_name,omitempty"`

	// AllowRead expose les outils de lecture au sous-agent ; AllowWrite
	// expose ceux d'écriture. Une écriture passe DE TOUTE FAÇON par la
	// confirmation humaine de l'hôte : ces interrupteurs décident
	// seulement de ce que l'agent voit.
	AllowRead  bool `json:"allow_read"`
	AllowWrite bool `json:"allow_write"`

	// EventStore revendique le magasin des rappels de ce membre. La clé
	// est réservée par l'hôte (pluginsdk.EventStoreConfigKey) : c'est le
	// seul champ de ce document qu'il lit.
	EventStore bool `json:"automata_event_store"`

	// TLSFingerprint est l'exception TLS du membre : l'empreinte SHA-256
	// du certificat qu'il a explicitement accepté, après l'avoir vu. Vide
	// — le cas normal — donne la vérification habituelle. Elle vaut pour
	// CE certificat seulement : un intermédiaire qui en présente un autre
	// reste refusé.
	TLSFingerprint string `json:"tls_fingerprint,omitempty"`

	// PollSeconds est la période de balayage des échéances ; 0 garde le
	// défaut.
	PollSeconds int `json:"poll_seconds,omitempty"`

	// LastSweep est le curseur du surveillant : les occurrences échues
	// jusqu'à cet instant (RFC 3339 UTC) ont déjà été annoncées. Sans lui,
	// un redémarrage rejouerait tous les rappels de la journée.
	LastSweep string `json:"last_sweep,omitempty"`
}

// defaultPollSeconds : une minute suffit pour la ponctualité d'un rappel
// humain et reste discret pour un serveur CalDAV partagé.
const defaultPollSeconds = 60

// secretKeyPassword est l'unique secret d'un compte.
const secretKeyPassword = "password"

// Vérification à la compilation : le champ EventStore doit porter très
// exactement la clé réservée par l'hôte. Une faute de frappe ici ne
// casserait rien de visible — le magasin ne serait simplement jamais
// utilisé, et les rappels partiraient ailleurs sans un mot.
var _ = mustMatchReservedKey()

func mustMatchReservedKey() bool {
	raw, err := json.Marshal(memberConfig{EventStore: true})
	if err != nil {
		panic(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		panic(err)
	}
	if _, ok := probe[pluginsdk.EventStoreConfigKey]; !ok {
		panic("memberConfig n'expose pas " + pluginsdk.EventStoreConfigKey)
	}
	return true
}

func parseConfig(raw string) (memberConfig, error) {
	var cfg memberConfig
	if raw == "" {
		return cfg, fmt.Errorf("no calendar configured")
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, fmt.Errorf("unreadable configuration")
	}
	return cfg, nil
}

// complete dit si le compte a de quoi joindre un serveur.
func (c memberConfig) complete() bool {
	return c.ServerURL != "" && c.Username != ""
}

func (c memberConfig) pollInterval() int {
	if c.PollSeconds > 0 {
		return c.PollSeconds
	}
	return defaultPollSeconds
}

func (c memberConfig) marshal() string {
	raw, _ := json.Marshal(c)
	return string(raw)
}
