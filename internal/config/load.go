// Package config gère le chargement et la validation de la configuration
// YAML d'Automata.
//
// La configuration doit être intégralement chargée et validée avant toute
// initialisation de service ou connexion externe (voir Load).
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load lit le fichier de configuration situé à path, expand les variables
// d'environnement, décode le YAML, charge les system prompts, résout les
// chemins relatifs par rapport au répertoire du fichier de configuration,
// puis valide intégralement la configuration obtenue.
//
// Toute erreur retournée agrège, dans la mesure du possible, l'ensemble des
// violations trouvées plutôt que la première seule.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lecture du fichier de configuration %q: %w", path, err)
	}

	expanded, err := expandEnvVars(raw)
	if err != nil {
		return nil, err
	}

	var cfg Config

	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("configuration YAML invalide: %w", err)
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("résolution du répertoire de configuration: %w", err)
	}

	applyDefaults(&cfg)
	resolvePaths(&cfg, baseDir)
	loadSystemPrompts(&cfg, baseDir)

	if err := Validate(&cfg, baseDir); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// applyDefaults applique les valeurs par défaut documentées de la
// configuration.
func applyDefaults(cfg *Config) {
	for i, sched := range cfg.Schedules {
		if sched.Concurrency.Policy == "" {
			cfg.Schedules[i].Concurrency.Policy = ConcurrencyPolicyForbid
		}
	}
}
