package main

import (
	"encoding/json"
	"slices"
)

// memberConfig est le réglage du membre : les entrées du catalogue qu'il a
// activées. Rien d'autre — ni URL, ni commande, ni en-tête. Ce que le
// membre décide, c'est de choisir parmi ce que l'exploitant propose ; ce
// qu'il apporte, ce sont ses identifiants, qui passent par SetSecret et ne
// figurent jamais ici.
type memberConfig struct {
	Enabled []string `json:"enabled"`
}

func parseConfig(raw string) memberConfig {
	var cfg memberConfig
	if raw == "" {
		return cfg
	}
	// Un document illisible se comporte comme un document vide : aucune
	// entrée activée. Refuser ici priverait la personne de son interface,
	// seul endroit d'où elle pourrait réparer.
	_ = json.Unmarshal([]byte(raw), &cfg)
	return cfg
}

func (c memberConfig) marshal() string {
	raw, _ := json.Marshal(c)
	return string(raw)
}

func (c memberConfig) enabled(name string) bool {
	return slices.Contains(c.Enabled, name)
}

// withEntry active ou désactive une entrée, en gardant la liste triée et
// sans doublon.
func (c memberConfig) withEntry(name string, on bool) memberConfig {
	next := slices.Clone(c.Enabled)
	next = slices.DeleteFunc(next, func(n string) bool { return n == name })
	if on {
		next = append(next, name)
	}
	slices.Sort(next)

	return memberConfig{Enabled: slices.Compact(next)}
}

// secretKey range l'identifiant d'une entrée sous un nom qui ne peut pas
// heurter celui d'une autre : deux entrées peuvent demander une clé
// « api_token » sans partager la même valeur.
func secretKey(agentName, credentialKey string) string {
	return agentName + "." + credentialKey
}
