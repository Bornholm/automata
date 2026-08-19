package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/delegation"
	"github.com/bornholm/automata/internal/model"
)

// newDescribeCapabilitiesTool expose describe_capabilities à
// l'orchestrateur : un instantané de ce que l'assistant sait faire au
// moment de l'appel — les outils réellement offerts dans ce tour et l'état
// effectif de chaque spécialiste (interrogé, pas recopié de la
// configuration). C'est la réponse honnête à « que sais-tu faire ? » : le
// prompt, lui, décrit une configuration qui peut être en panne.
//
// turnTools est la liste des outils du tour COURANT, capturée après leur
// assemblage : elle reflète les permissions et la configuration de cette
// requête précise, pas une liste générale.
func newDescribeCapabilitiesTool(turnTools []llm.Tool, specialists map[string]delegation.Specialist, descriptions map[string]string, identity model.ExecutionIdentity) llm.Tool {
	return llm.NewFuncTool(
		"describe_capabilities",
		"Report what you can actually do RIGHT NOW: your currently available tools and the live status of each specialist. Call this before answering any question about your own capabilities — never answer from assumptions.",
		llm.NewJSONSchema(),
		func(ctx context.Context, params map[string]any) (llm.ToolResult, error) {
			var b strings.Builder

			b.WriteString("## Tools available in this conversation\n")

			for _, tool := range turnTools {
				// L'outil d'introspection lui-même n'apporte rien à la
				// réponse : le décrire inviterait le modèle à des appels
				// récursifs sans information nouvelle.
				if tool.Name() == "describe_capabilities" {
					continue
				}

				fmt.Fprintf(&b, "- %s\n", tool.Name())
			}

			if len(specialists) == 0 {
				b.WriteString("\nNo specialist is configured.\n")
				return llm.NewToolResult(b.String()), nil
			}

			b.WriteString("\n## Specialists (live status)\n")

			names := make([]string, 0, len(specialists))
			for name := range specialists {
				names = append(names, name)
			}
			sort.Strings(names)

			for _, name := range names {
				description := descriptions[name]
				if description == "" {
					description = "no description configured"
				}

				reporter, ok := specialists[name].(delegation.CapabilityReporter)
				if !ok {
					fmt.Fprintf(&b, "- %s: %s\n", name, description)
					continue
				}

				report := reporter.ReportCapabilities(ctx, identity)

				switch {
				case !report.Available:
					detail := report.Detail
					if detail == "" {
						detail = "currently unavailable"
					}
					fmt.Fprintf(&b, "- %s: %s — UNAVAILABLE right now (%s)\n", name, description, detail)
				case len(report.Tools) > 0:
					fmt.Fprintf(&b, "- %s: %s — available, tools: %s\n", name, description, strings.Join(report.Tools, ", "))
				default:
					fmt.Fprintf(&b, "- %s: %s — available\n", name, description)
				}
			}

			return llm.NewToolResult(b.String()), nil
		},
	)
}
