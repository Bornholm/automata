package config

import (
	"os"
	"path/filepath"
)

// resolvePath résout un chemin relatif par rapport à baseDir. Un chemin
// absolu ou vide est retourné inchangé.
func resolvePath(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}

	return filepath.Join(baseDir, p)
}

// resolvePaths résout, en les mutant, tous les chemins relatifs de la
// configuration par rapport au répertoire du fichier de configuration.
func resolvePaths(cfg *Config, baseDir string) {
	cfg.Storage.Application.Path = resolvePath(baseDir, cfg.Storage.Application.Path)
	cfg.Plugins.Dir = resolvePath(baseDir, cfg.Plugins.Dir)
	cfg.Memory.Store.Path = resolvePath(baseDir, cfg.Memory.Store.Path)

	for i := range cfg.Memory.Indexes {
		cfg.Memory.Indexes[i].Path = resolvePath(baseDir, cfg.Memory.Indexes[i].Path)
	}

	for name, provider := range cfg.Courier.Providers {
		if sessionPath, ok := provider.Extra["session_path"].(string); ok {
			provider.Extra["session_path"] = resolvePath(baseDir, sessionPath)
			cfg.Courier.Providers[name] = provider
		}
	}

	// Les sauvegardes suivent la même règle que le reste : un chemin
	// relatif se lit depuis le fichier de configuration, pas depuis le
	// répertoire de lancement — sinon les copies atterrissent où le
	// service a été démarré, et les bases annexes restent introuvables.
	cfg.Backup.Directory = resolvePath(baseDir, cfg.Backup.Directory)
	for name, path := range cfg.Backup.ExtraPaths {
		cfg.Backup.ExtraPaths[name] = resolvePath(baseDir, path)
	}
}

// loadSystemPrompts résout le chemin des prompts basés sur un fichier et
// charge leur contenu en mémoire. Les erreurs de lecture ne sont pas
// remontées ici : elles sont détectées par Validate, qui vérifie
// l'existence et la lisibilité du fichier.
func loadSystemPrompts(cfg *Config, baseDir string) {
	for name, agent := range cfg.Agents {
		sp := loadSystemPrompt(agent.SystemPrompt, baseDir)

		for orgID, override := range sp.OrgOverrides {
			sp.OrgOverrides[orgID] = loadSystemPrompt(override, baseDir)
		}

		agent.SystemPrompt = sp
		cfg.Agents[name] = agent
	}
}

// loadSystemPrompt résout et charge une source de prompt (fichier ou
// inline) sans toucher à ses éventuels org_overrides, traités par
// l'appelant.
func loadSystemPrompt(sp SystemPrompt, baseDir string) SystemPrompt {
	switch {
	case sp.File != "" && sp.Inline == "":
		resolved := resolvePath(baseDir, sp.File)
		sp.File = resolved

		if content, err := os.ReadFile(resolved); err == nil {
			sp.Content = string(content)
		}
	case sp.File == "" && sp.Inline != "":
		sp.Content = sp.Inline
	case sp.File != "" && sp.Inline != "":
		sp.File = resolvePath(baseDir, sp.File)
	}

	return sp
}
