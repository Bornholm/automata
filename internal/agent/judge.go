package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bornholm/genai/llm"

	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/media"
	"github.com/bornholm/automata/internal/model"
)

// Le juge.
//
// Un tour qui n'appelle aucun outil alors qu'on lui en offre n'a rien
// observé. Ce fait est certain, et l'hôte le connaît (voir
// answeredWithoutTools). Ce qu'il ne sait pas lire, c'est ce que la réponse
// PRÉTEND : « le service de calendrier est indisponible » et « je n'ai pas
// d'idée pour ce soir » sont deux textes sans outil, et un seul des deux est
// un mensonge.
//
// Un second modèle est appelé pour cette lecture-là, et pour elle seule. Il
// reçoit la demande, la réponse, et le fait qu'aucun outil n'a été appelé.
// Il ne reçoit NI l'historique, NI les outils : il n'a rien à vérifier dans
// le monde, seulement à dire si le texte affirme quelque chose que rien
// n'appuie.
//
// C'est bien un jugement, donc faillible — d'où le cadre :
//
//   - il n'est consulté que lorsque le signal structurel est tombé. Une
//     preuve décide QUAND, une opinion décide QUOI ;
//   - son verdict ne réécrit jamais la réponse : il déclenche UNE relance du
//     modèle d'origine, à qui la raison est transmise, ses outils toujours
//     offerts ;
//   - un juge absent, en panne ou illisible laisse le tour passer inchangé.
//     Une vérification indisponible ne doit pas coûter sa réponse à la
//     personne qui attend.

// Judge rend un avis sur une réponse produite sans aucun appel d'outil.
// Implémenté dans internal/registry sur le catalogue de modèles ; sa valeur
// nil désactive la vérification.
type Judge interface {
	// ReviewGrounding retourne l'avis, ou une erreur si le juge n'a pas pu
	// se prononcer. orgID sélectionne le modèle du rôle « judge ».
	ReviewGrounding(ctx context.Context, orgID model.OrgID, request, reply string) (Grounding, error)
}

// Grounding est l'avis du juge.
type Grounding struct {
	// Grounded est faux quand la réponse affirme un fait — une
	// indisponibilité, un échec, un résultat — que rien n'appuie.
	Grounded bool `json:"grounded"`
	// Reason dit ce qui est affirmé sans appui, en une phrase. Elle est
	// transmise TELLE QUELLE au modèle d'origine : c'est elle qui lui
	// apprend quoi corriger, et « la réponse n'est pas fondée » ne lui
	// apprend rien.
	Reason string `json:"reason"`
}

// JudgeSystemPrompt part au juge : anglais, comme toute consigne destinée à
// un modèle (AGENTS.md).
//
// Il est écrit pour un modèle SANS contexte : ni historique, ni outils, ni
// connaissance de l'installation. Tout ce qu'il peut faire est de lire deux
// textes et de dire si le second affirme ce que personne n'a vérifié — la
// consigne le borne donc explicitement, sans quoi il jugerait le ton, la
// longueur ou l'utilité.
const JudgeSystemPrompt = `You check one thing, and nothing else.

You are given a user's request and the assistant's reply. You are also told a fact you can rely on entirely: while writing that reply, the assistant called NO tool at all, although tools were available to it. It therefore looked nothing up, tried nothing, and observed nothing.

Answer this: does the reply assert something that this makes impossible to know?

Set "grounded" to false when the reply states, as if observed:
  - that a service, server, tool or specialist is unavailable, down, unreachable or failing;
  - that an attempt was made and failed;
  - that an action was performed, saved, scheduled, sent or cancelled;
  - a concrete result — a link, a date, an appointment, a figure, a quotation — presented as retrieved rather than as a guess.

Set "grounded" to true when the reply claims none of that: small talk, an opinion, general knowledge, a clarifying question, or an honest statement that it cannot do something and did not try.

When "grounded" is false, "reason" must name the exact claim, in one sentence, addressed to the assistant — for example: "you stated the calendar service is unavailable, but you never called a calendar tool". When "grounded" is true, leave "reason" empty.

Judge only this. Never comment on tone, length, style or usefulness.`

// NewGroundingSchema contraint la sortie du juge : deux champs, pas de
// prose autour. Sans schéma, un modèle rend « Voici mon analyse : … » et
// l'avis n'est plus lisible par programme.
//
// Exporté : c'est internal/registry qui appelle le modèle du rôle « judge »,
// et il doit poser le MÊME schéma que celui que parseGrounding attend.
func NewGroundingSchema() llm.ResponseSchema {
	return llm.NewResponseSchema("grounding", "Whether the reply asserts something it could not know.", map[string]any{
		"type": "object",
		"properties": map[string]any{
			"grounded": map[string]any{
				"type":        "boolean",
				"description": "False when the reply asserts, as observed, something no tool call could have established.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "The unfounded claim, in one sentence addressed to the assistant. Empty when grounded is true.",
			},
		},
		"required":             []string{"grounded", "reason"},
		"additionalProperties": false,
	})
}

// judgePrompt compose ce que le juge lit. La demande et la réponse sont
// encadrées : un modèle qui reçoit deux textes collés en juge un seul.
func judgePrompt(request, reply string) string {
	var b strings.Builder

	b.WriteString("<user_request>\n")
	b.WriteString(strings.TrimSpace(request))
	b.WriteString("\n</user_request>\n\n<assistant_reply>\n")
	b.WriteString(strings.TrimSpace(reply))
	b.WriteString("\n</assistant_reply>")

	return b.String()
}

// parseGrounding lit l'avis. Un texte illisible n'est pas un verdict : il
// remonte en erreur, et le tour passe inchangé.
//
// La lecture est volontairement tolérante. Le schéma JSON est demandé au
// modèle, mais il n'est pas toujours respecté — et le 4 septembre 2026, un
// avis rendu hors schéma a laissé passer une réponse inventée sur l'agenda,
// alors que le juge était là pour ça. llm.ParseJSON isole le bloc entre
// accolades dans le texte et le répare avant de le lire : clôture Markdown,
// virgule finale, guillemets simples, phrase d'introduction. Ce qui reste
// illisible remonte AVEC le brut tronqué, sans quoi la prochaine panne se
// diagnostiquera encore à la première lettre du message d'erreur.
func parseGrounding(raw string) (Grounding, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Grounding{}, fmt.Errorf("agent: avis du juge vide")
	}

	// Un avis sans la moindre accolade — « grounded: false » sur deux
	// lignes — n'offre aucun bloc à isoler. Les accolades posées autour du
	// tout donnent à la réparation de quoi travailler ; si ce n'était pas
	// un objet déguisé, la lecture échoue comme avant.
	candidate := raw
	if !strings.ContainsRune(candidate, '{') {
		candidate = "{" + candidate + "}"
	}

	items, err := llm.ParseJSON[groundingPayload](llm.NewMessage(llm.RoleAssistant, candidate))
	if err != nil {
		return Grounding{}, fmt.Errorf("agent: avis du juge illisible (%s): %w", truncateForLog(raw), err)
	}
	// Aucun bloc trouvé : llm.ParseJSON rend alors une liste vide SANS
	// erreur. Et une phrase libre entourée d'accolades se répare en objet
	// VIDE, qui vaudrait « grounded: false » — un verdict négatif tiré d'un
	// texte qui n'en portait aucun. Le verdict n'existe que si le champ
	// était là, d'où le pointeur.
	if len(items) == 0 || items[0].Grounded == nil {
		return Grounding{}, fmt.Errorf("agent: avis du juge illisible (%s): aucun verdict dans la réponse", truncateForLog(raw))
	}

	return Grounding{Grounded: *items[0].Grounded, Reason: items[0].Reason}, nil
}

// groundingPayload est l'avis tel qu'il arrive du modèle. Grounded y est un
// pointeur pour distinguer « le juge a répondu faux » de « le juge n'a rien
// répondu du tout » : confondre les deux relancerait le modèle sur un
// verdict que personne n'a rendu.
type groundingPayload struct {
	Grounded *bool  `json:"grounded"`
	Reason   string `json:"reason"`
}

// truncateForLog borne un brut de modèle destiné à une ligne de journal.
func truncateForLog(raw string) string {
	const max = 200

	raw = strings.Join(strings.Fields(raw), " ")
	if len(raw) <= max {
		return raw
	}

	return raw[:max] + "…"
}

// ungroundedNotice est la consigne de la relance. Elle transporte la raison
// du juge, et rouvre les deux mêmes issues que partout ailleurs : appeler
// l'outil, ou dire honnêtement ce qui n'a pas été fait.
//
// En anglais, comme tout ce qui part au modèle.
const ungroundedNotice = `[Note from the system, not from the person you are talking to.]

A first attempt at answering the request above was discarded before anyone saw it. It asserted something you could not know: %s

No tool had been called, so nothing had been observed — that claim was a guess.

Answer the request above now, one of two ways and no other:
  - if a tool offered to you can settle the question, CALL IT NOW and answer from what it returns;
  - otherwise, answer without that claim, and say plainly what you did not do.

Write to the person, as if for the first time: they never saw the discarded attempt and are waiting for their answer. Do not mention this note, do not apologise, do not ask permission to use a tool you already have — use it.`

// defaultJudgeAttempts est le nombre d'appels au juge pour un tour, faute de
// réglage. Trois : la panne observée est intermittente — un modèle qui rend
// son avis hors schéma une fois sur deux —, et deux essais de plus suffisent
// à la traverser sans faire attendre la personne pour rien.
const defaultJudgeAttempts = 3

// ErrJudgeUnavailable est rendue quand le juge n'a pas pu se prononcer après
// tous ses essais. Le tour échoue : l'ingress la journalise en ERREUR — c'est
// une panne, l'alerting doit la voir — et envoie ingress.FallbackReply, qui
// invite à réessayer.
var ErrJudgeUnavailable = errors.New("agent: juge indisponible après tous les essais")

// reviewGrounding fait relire la réponse par le juge lorsque le tour n'a
// appelé aucun outil, et relance le modèle UNE fois si l'avis est négatif.
//
// Ce qui reste sans conséquence : pas de juge du tout, verdict sans raison,
// relance en échec. Le tour passe alors inchangé.
//
// Ce qui coûte le tour : un juge qui ne se prononce pas, après tous ses
// essais. Le fait qui a DÉCLENCHÉ la relecture est certain — des outils
// étaient offerts, aucun n'a été appelé —, et la réponse qui reste sur les
// bras est précisément celle dont personne ne peut dire si elle est inventée.
// Le 4 septembre 2026, rendre ce texte tel quel a donné à quelqu'un un
// agenda vide qui ne l'était pas. Mieux vaut demander de réessayer.
func (a *OrchestratorAgent) reviewGrounding(
	ctx context.Context,
	client llm.ChatCompletionClient,
	identity model.ExecutionIdentity,
	messages []llm.Message,
	tools []llm.Tool,
	maxIterations int,
	input string,
	result toolLoopResult,
) (toolLoopResult, error) {
	if a.judge == nil || !answeredWithoutTools(tools, result) || strings.TrimSpace(result.Text) == "" {
		return result, nil
	}

	grounding, err := a.askJudge(ctx, identity, input, result.Text)
	if err != nil {
		return toolLoopResult{}, err
	}
	if grounding.Grounded {
		return result, nil
	}

	reason := strings.TrimSpace(grounding.Reason)
	if reason == "" {
		// Un verdict négatif sans raison n'apprend rien au modèle : sans
		// elle, la relance revient à « recommence », et il recommence à
		// l'identique.
		if a.logger != nil {
			a.logger.WarnContext(ctx, "agent: verdict du juge sans raison, réponse rendue telle quelle",
				"agent", a.agentName)
		}

		return result, nil
	}

	if a.logger != nil {
		a.logger.WarnContext(ctx, "agent: réponse jugée non fondée, relance du modèle",
			"agent", a.agentName,
			"raison", reason,
			"remède", "le modèle affecté à ce rôle affirme sans appeler ses outils")
	}
	a.metrics.IncUngroundedReply()

	// Le tour est REJOUÉ, brouillon fautif exclu : le lui remontrer serait
	// lui donner le texte à imiter, et sa réponse s'adresserait à la
	// consigne plutôt qu'à la personne (voir replay.go).
	retryMessages := replayWithNotice(messages, strings.Replace(ungroundedNotice, "%s", reason, 1))

	retried, err := runToolLoop(ctx, client, retryMessages, tools, maxIterations,
		a.maxToolContextBytes, ErrMaxDelegationsReached, a.logger, a.agentName, "")
	if err != nil {
		if a.logger != nil {
			a.logger.WarnContext(ctx, "agent: relance après verdict du juge en échec, réponse initiale conservée",
				"agent", a.agentName, "error", err)
		}

		return result, nil
	}

	// Les résultats d'outils du tour initial restent des sources pour la
	// suite (contrôle des adresses) : la relance s'ajoute, elle ne remplace
	// pas.
	retried.ToolResults = append(append([]string(nil), result.ToolResults...), retried.ToolResults...)
	retried.Attachments = append(append([]media.Media(nil), result.Attachments...), retried.Attachments...)

	if a.logger != nil {
		a.logger.InfoContext(ctx, "agent: réponse reprise après verdict du juge",
			"agent", a.agentName, "tool_calls", retried.ToolCalls)
	}

	return retried, nil
}

// askJudge demande l'avis, et le redemande tant qu'il reste des essais. Un
// juge qui rend un avis illisible le rend souvent par intermittence : le
// même texte relu par le même modèle passe à l'essai suivant, et cela ne
// coûte qu'un aller-retour.
//
// L'annulation du contexte, elle, ne se réessaie pas : plus personne
// n'attend la réponse.
func (a *OrchestratorAgent) askJudge(
	ctx context.Context,
	identity model.ExecutionIdentity,
	input string,
	reply string,
) (Grounding, error) {
	attempts := a.judgeAttempts
	if attempts <= 0 {
		attempts = defaultJudgeAttempts
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		grounding, err := a.judge.ReviewGrounding(ctx, identity.OrgID, input, reply)
		if err == nil {
			return grounding, nil
		}
		lastErr = err

		if a.logger != nil {
			a.logger.WarnContext(ctx, "agent: avis du juge non rendu",
				"agent", a.agentName, "essai", attempt, "essais", attempts, "error", err)
		}

		if ctx.Err() != nil {
			break
		}
	}

	if a.logger != nil {
		a.logger.ErrorContext(ctx, "agent: juge indisponible, tour abandonné",
			"agent", a.agentName,
			"essais", attempts,
			"error", lastErr,
			"remède", "réponse non vérifiable rendue au lieu d'être envoyée ; vérifier le modèle du rôle judge")
	}
	a.metrics.IncUnverifiedReply()

	return Grounding{}, fmt.Errorf("%w (%d essais): %w", ErrJudgeUnavailable, attempts, lastErr)
}

// resolvedJudge est le juge adossé au catalogue de modèles : il sert le
// modèle du rôle « judge », choisi par organisation comme tous les autres.
//
// Aucun modèle affecté au rôle = aucune relecture. C'est une erreur
// remontée, pas une panne : reviewGrounding la journalise et rend la
// réponse inchangée.
type resolvedJudge struct {
	resolver ClientResolver
}

// NewJudge construit le juge sur un résolveur de modèles. Retourne nil si
// le résolveur l'est : sans catalogue, pas de second modèle à interroger,
// et le tour se déroule exactement comme avant.
func NewJudge(resolver ClientResolver) Judge {
	if resolver == nil {
		return nil
	}

	return &resolvedJudge{resolver: resolver}
}

// ReviewGrounding implémente Judge.
func (j *resolvedJudge) ReviewGrounding(ctx context.Context, orgID model.OrgID, request, reply string) (Grounding, error) {
	resolved, err := j.resolver.ResolveClient(ctx, llmclients.RoleJudge, orgID)
	if err != nil {
		return Grounding{}, fmt.Errorf("agent: modèle du rôle judge non résolu: %w", err)
	}

	// Ni historique, ni outils : le juge n'a rien à poursuivre et rien à
	// vérifier dans le monde. Deux textes, une question.
	resp, err := resolved.Client.ChatCompletion(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, JudgeSystemPrompt),
			llm.NewMessage(llm.RoleUser, judgePrompt(request, reply)),
		),
		llm.WithJSONResponse(NewGroundingSchema()),
	)
	if err != nil {
		return Grounding{}, fmt.Errorf("agent: appel du juge: %w", err)
	}

	msg := resp.Message()
	if msg == nil {
		return Grounding{}, fmt.Errorf("agent: avis du juge vide")
	}

	return parseGrounding(msg.Content())
}
