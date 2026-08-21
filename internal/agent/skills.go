package agent

import (
	"context"
	"strings"

	"github.com/bornholm/genai/llm"
)

// Compétences (« skills ») : des modes opératoires écrits une fois et
// chargés à la demande, sur le patron de la divulgation progressive.
// L'agent voit en permanence un CATALOGUE d'une ligne par compétence, et
// n'en paie le contenu complet que lorsqu'il appelle load_skill.
//
// Tout ce qui suit part au modèle : catalogue, nom, description et
// résultats de l'outil s'écrivent en ANGLAIS.

// SkillSummary est l'entrée de catalogue montrée au modèle.
type SkillSummary struct {
	Name        string
	Description string
}

// SkillsProvider fournit les compétences actives pour un agent donné.
// Implémenté par internal/skills ; nil-safe partout — une instance sans
// compétence n'a ni catalogue ni outil monté.
type SkillsProvider interface {
	// SkillsFor retourne le catalogue visible de cet agent.
	SkillsFor(ctx context.Context, agentName string) []SkillSummary
	// LoadSkill retourne le contenu markdown d'une compétence. Le
	// ciblage et l'activation y sont revérifiés : un nom deviné par le
	// modèle pour un agent non ciblé reste introuvable.
	LoadSkill(ctx context.Context, agentName, skillName string) (content string, found bool)
}

// skillsCatalogSection compose le bloc à ajouter au prompt système du
// tour. Retourne la chaîne vide quand le catalogue est vide : aucun
// budget de tokens n'est dépensé pour une bibliothèque inexistante.
func skillsCatalogSection(summaries []SkillSummary) string {
	if len(summaries) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Skills\n\n")
	b.WriteString("Before improvising a multi-step procedure, check this catalog. ")
	b.WriteString("If a skill matches the task, call load_skill with its name and follow the instructions it returns.\n\n")
	for _, s := range summaries {
		b.WriteString("- ")
		b.WriteString(s.Name)
		if s.Description != "" {
			b.WriteString(" — ")
			b.WriteString(s.Description)
		}
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// newLoadSkillTool construit l'outil load_skill pour un agent donné. Le
// nom de l'agent est capturé en closure : le modèle désigne une
// compétence, il ne choisit jamais pour le compte de qui.
//
// Aucun appel LLM ni réseau : une lecture en base, un retour direct.
func newLoadSkillTool(provider SkillsProvider, agentName string) llm.Tool {
	schema := llm.NewJSONSchema().
		RequiredProperty("name", "Exact name of the skill, as listed in your catalog.", "string")

	return llm.NewFuncTool(
		"load_skill",
		"Load the full instructions of a skill from the catalog. Returns markdown to follow.",
		schema,
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			name, _ := params["name"].(string)
			name = strings.TrimSpace(name)
			if name == "" {
				return llm.NewToolResult("error: 'name' is required."), nil
			}

			content, found := provider.LoadSkill(ctx, agentName, name)
			if !found {
				// Échec métier : un résultat d'outil, jamais une erreur Go
				// qui avorterait le tour.
				return llm.NewToolResult("error: no skill named \"" + name + "\" is available to you."), nil
			}

			return llm.NewToolResult(content), nil
		},
	).WithReadOnlyHint(true)
}

// appendSkills complète le prompt système et la liste d'outils du tour
// avec le catalogue et load_skill, si le provider a quelque chose à
// offrir à cet agent. Partagé par les trois agents équipés
// (orchestrateur, spécialiste MCP, sous-agent de plugin) : le montage se
// fait PAR TOUR, jamais au démarrage — la bibliothèque vit.
func appendSkills(ctx context.Context, provider SkillsProvider, agentName, systemPrompt string, tools []llm.Tool) (string, []llm.Tool) {
	if provider == nil {
		return systemPrompt, tools
	}

	section := skillsCatalogSection(provider.SkillsFor(ctx, agentName))
	if section == "" {
		return systemPrompt, tools
	}

	return systemPrompt + "\n\n" + section, append(tools, newLoadSkillTool(provider, agentName))
}
