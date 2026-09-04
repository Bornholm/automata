package agent

import (
	"context"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/media"
)

// Une URL que personne n'a fournie.
//
// Un modèle qui n'appelle pas son outil ne dit pas toujours « je ne peux
// pas » : il lui arrive de FABRIQUER ce que l'outil aurait rendu. Vu en
// production le 2026-09-03, deux fois : « Voici votre lien :
// https://profile.cadoles.com/?t=8c2a4f9d1e7b5c3a », d'une forme
// plausible, d'un domaine plausible, et entièrement inventé. La personne
// clique, et rien n'existe au bout.
//
// Le contrôle est structurel, et ne lit aucun sens. Une URL présente dans
// une réponse doit venir de quelque part : du message de la personne, de
// l'historique, ou d'un résultat d'outil de ce tour. Une URL qui n'est dans
// aucune de ces trois sources n'a été vue par personne — le modèle l'a
// composée.
//
// Ce que l'hôte en fait n'est PAS de trancher à la place du modèle : lui
// demander « est-elle légitime ? » ne prouverait rien, celui qui invente
// sait aussi affirmer. Il la lui rend une fois, en lui disant ce qui
// manque, avec ses outils toujours offerts — c'est exactement ce qui suffit
// en conversation, où « utilise l'outil » débloque la situation. Puis il
// REVÉRIFIE, structurellement, la réponse obtenue : c'est la vérification
// qui décide, jamais la déclaration du modèle.

// unsourcedURLNotice est la consigne de la relance. En anglais, comme tout
// ce qui part au modèle (AGENTS.md).
//
// Elle nomme les URL en cause plutôt que de parler en général : « une de
// tes URL » laisse le modèle choisir laquelle corriger, et il choisit mal.
const unsourcedURLNotice = `[Note from the system, not from the person you are talking to.]

A first attempt at answering the request above was discarded before anyone saw it. It contained a web address that appears nowhere in this conversation and that no tool had returned: %s

Nobody showed you that address — it was composed. An address built this way does not work, and the person would click it.

Answer the request above now, one of two ways and no other:
  - if a tool offered to you can produce this address, CALL IT NOW and use exactly what it returns;
  - otherwise, answer without that address, and say plainly what you could not provide.

Write to the person, as if for the first time: they never saw the discarded attempt and are waiting for their answer. Do not mention this note, do not apologise, do not ask permission to use a tool you already have — use it.`

// unsourcedURLs retourne les URL de reply qui n'apparaissent dans aucune
// des sources, dans l'ordre de première apparition et sans doublon.
//
// La comparaison porte sur l'URL nettoyée de sa ponctuation finale : une
// même adresse citée « … (https://x.test/a). » et rendue par un outil sans
// parenthèses est la même adresse.
func unsourcedURLs(reply string, sources ...string) []string {
	found := extractReferences([]string{reply})
	if len(found) == 0 {
		return nil
	}

	known := make(map[string]bool)
	for _, source := range sources {
		for _, url := range extractReferences([]string{source}) {
			known[url] = true
		}
	}

	var unsourced []string
	for _, url := range found {
		if !known[url] {
			unsourced = append(unsourced, url)
		}
	}

	return unsourced
}

// messageTexts rassemble le texte des messages du tour : le prompt système,
// l'historique et la demande. C'est le premier gisement de sources
// légitimes — une adresse que la personne vient d'écrire est la sienne, pas
// une invention.
func messageTexts(messages []llm.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, m := range messages {
		texts = append(texts, m.Content())
	}
	return texts
}

// verifyURLSources relance le modèle UNE fois lorsque sa réponse contient
// une URL que rien ne lui a fournie, puis revérifie ce qu'il rend.
//
// Une seule relance : au-delà, un modèle qui persiste persistera, et
// chaque tentative coûte une complétion à la personne qui attend. Si
// l'adresse inventée survit à la relance, la réponse est rendue telle
// quelle et l'incident journalisé — l'hôte ne réécrit pas le texte de
// quelqu'un d'autre sur un soupçon, mais l'exploitant doit savoir que le
// modèle affecté au rôle fabrique des adresses.
func (a *OrchestratorAgent) verifyURLSources(
	ctx context.Context,
	client llm.ChatCompletionClient,
	messages []llm.Message,
	tools []llm.Tool,
	maxIterations int,
	result toolLoopResult,
) toolLoopResult {
	sources := append(messageTexts(messages), result.ToolResults...)

	unsourced := unsourcedURLs(result.Text, sources...)
	if len(unsourced) == 0 {
		return result
	}

	a.logURLIncident(ctx, "agent: adresse fabriquée dans la réponse, relance du modèle", unsourced, result)

	// Même règle que pour le juge : on rejoue la demande sans montrer le
	// brouillon (voir replay.go).
	retryMessages := replayWithNotice(messages, strings.Replace(unsourcedURLNotice, "%s", strings.Join(unsourced, ", "), 1))

	retried, err := runToolLoop(ctx, client, retryMessages, tools, maxIterations,
		a.maxToolContextBytes, ErrMaxDelegationsReached, a.logger, a.agentName, "")
	if err != nil {
		// La relance a échoué : la réponse initiale vaut mieux que rien,
		// et la personne a au moins un texte.
		a.logURLIncident(ctx, "agent: relance après adresse fabriquée en échec", unsourced, result)
		return result
	}

	// Ce qui décide n'est pas ce que le modèle affirme, c'est ce que la
	// nouvelle réponse contient : les résultats d'outils du tour de relance
	// s'ajoutent aux sources, et une adresse enfin obtenue par l'outil est
	// donc légitime.
	retried.ToolResults = append(append([]string(nil), result.ToolResults...), retried.ToolResults...)
	retried.Attachments = append(append([]media.Media(nil), result.Attachments...), retried.Attachments...)

	if remaining := unsourcedURLs(retried.Text, append(sources, retried.ToolResults...)...); len(remaining) > 0 {
		a.logURLIncident(ctx, "agent: adresse fabriquée maintenue après relance", remaining, retried)
	}

	return retried
}

// logURLIncident journalise sans jamais recopier la réponse : l'adresse
// suffit à comprendre, et le texte d'une conversation n'a rien à faire dans
// le flux de l'exploitant.
func (a *OrchestratorAgent) logURLIncident(ctx context.Context, msg string, urls []string, result toolLoopResult) {
	if a.logger == nil {
		return
	}

	a.logger.WarnContext(ctx, msg,
		"agent", a.agentName,
		"urls", strings.Join(urls, ", "),
		"tool_calls", result.ToolCalls,
		"remède", "le modèle affecté à ce rôle compose des adresses au lieu d'appeler ses outils")
}
