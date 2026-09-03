package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/bornholm/automata/internal/llmclients"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// Administration du catalogue de modèles (migration 0022). La base fait
// autorité : une modification enregistrée ici prend effet au message
// suivant, sans redémarrage — le pool de clients relit chaque définition et
// reconstruit celles qui ont changé (internal/llmclients.Pool).
//
// Journaux : nom du client, fournisseur et modèle. JAMAIS la clé d'API, qui
// n'est d'ailleurs jamais relue par ces écrans — l'opérateur ne peut que la
// remplacer, comme pour les secrets de plugins.

// llmClientNamePattern borne le nom d'un client : c'est une clé référencée
// par les rôles et par les choix des organisations, et elle voyage dans une
// URL.
var llmClientNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// llmProviders énumère les fournisseurs acceptés par famille. Rejeter ici
// ce que les constructeurs rejetteraient plus tard évite d'enregistrer un
// client qui ne pourra jamais servir.
var llmProviders = map[string][]string{
	persistence.LLMClientKindLLM:   {"openai", "mistral", "openrouter"},
	persistence.LLMClientKindImage: {"openai", "openrouter", "minimax"},
}

// llmEfforts énumère les valeurs d'effort de réflexion acceptées.
var llmEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh"}

// HandleLLMClients liste le catalogue.
func (h *Handlers) HandleLLMClients(w http.ResponseWriter, r *http.Request) {
	page := view.LLMClientsPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}

	if h.LLMBox == nil {
		page.Error = "Le catalogue est en lecture seule : le secret de session ne permet pas d'ouvrir les clés enregistrées."
	}

	var rows []persistence.LLMClient
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		rows, err = h.LLMClients.List(r.Context(), tx, "")
		return err
	}) {
		return
	}

	// Les rôles servis par défaut se lisent en base (org_id vide) : ce
	// sont eux qui disent à quoi sert réellement chaque entrée du
	// catalogue — le fichier de configuration ne les connaît plus.
	var instanceDefaults map[string]string
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		instanceDefaults, err = h.OrgClients.ListByOrg(r.Context(), tx, "")
		return err
	}) {
		return
	}
	byClient := map[string][]string{}
	for role, name := range instanceDefaults {
		byClient[name] = append(byClient[name], role)
	}
	for _, roles := range byClient {
		sort.Strings(roles)
	}

	// Les rôles de l'instance, éditables sur cette même page : c'est ici
	// que se décide quel modèle sert chaque agent par défaut.
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		page.Roles, err = h.modelRoleRows(r.Context(), tx, "")
		return err
	}) {
		return
	}

	for _, row := range rows {
		page.Clients = append(page.Clients, view.LLMClientRow{
			Name:      row.Name,
			Kind:      row.Kind,
			Provider:  row.Provider,
			Model:     row.Model,
			Roles:     strings.Join(byClient[row.Name], ", "),
			HasKey:    row.APIKey != "",
			UpdatedAt: view.FormatShortDate(row.UpdatedAt),
		})
	}

	h.Render(w, r, http.StatusOK, view.AdminLLMClients(page))
}

// HandleLLMClientNewForm affiche le formulaire de création.

// HandleLLMClientNewForm affiche le formulaire de création.
func (h *Handlers) HandleLLMClientNewForm(w http.ResponseWriter, r *http.Request) {
	h.Render(w, r, http.StatusOK, view.AdminLLMClientForm(view.LLMClientFormPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		New:       true,
		Kind:      persistence.LLMClientKindLLM,
		Vision:    true,
	}))
}

// llmClientForm lit le formulaire commun à la création et à l'édition.

// llmClientForm lit le formulaire commun à la création et à l'édition.
func (h *Handlers) llmClientForm(r *http.Request, isNew bool) view.LLMClientFormPage {
	kind := strings.TrimSpace(r.PostFormValue("kind"))
	if kind != persistence.LLMClientKindImage {
		kind = persistence.LLMClientKindLLM
	}

	return view.LLMClientFormPage{
		Platforms:       h.SidebarPlatforms(),
		New:             isNew,
		Name:            strings.TrimSpace(r.PostFormValue("name")),
		Kind:            kind,
		Provider:        strings.TrimSpace(r.PostFormValue("provider")),
		Model:           strings.TrimSpace(r.PostFormValue("model")),
		BaseURL:         strings.TrimSpace(r.PostFormValue("base_url")),
		ReasoningEffort: strings.TrimSpace(r.PostFormValue("reasoning_effort")),
		Vision:          r.PostFormValue("vision") != "",
		ExtraFields:     strings.TrimSpace(r.PostFormValue("extra_fields")),
	}
}

// validateLLMClientForm vérifie ce qui empêcherait le client de servir.
// Retourne le message à afficher, ou "" si tout va bien.

// validateLLMClientForm vérifie ce qui empêcherait le client de servir.
// Retourne le message à afficher, ou "" si tout va bien.
func validateLLMClientForm(form view.LLMClientFormPage) string {
	if form.Provider == "" || form.Model == "" {
		return "Le fournisseur et le modèle sont requis."
	}

	providers, ok := llmProviders[form.Kind]
	if !ok {
		return "Usage inconnu."
	}
	if !slicesContains(providers, form.Provider) {
		return fmt.Sprintf("Fournisseur %q non supporté pour cet usage : %s.", form.Provider, strings.Join(providers, ", "))
	}

	if form.ReasoningEffort != "" && !slicesContains(llmEfforts, form.ReasoningEffort) {
		return fmt.Sprintf("Effort de réflexion %q inconnu : %s.", form.ReasoningEffort, strings.Join(llmEfforts, ", "))
	}

	if form.ExtraFields != "" {
		var fields map[string]any
		if err := json.Unmarshal([]byte(form.ExtraFields), &fields); err != nil {
			return "Les champs supplémentaires doivent former un objet JSON valide."
		}
	}

	return ""
}

// slicesContains dit si values contient value.

// slicesContains dit si values contient value.
func slicesContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// sealedKey retourne la valeur à écrire dans la colonne api_key : la
// nouvelle clé scellée si l'opérateur en a saisi une, l'ancienne sinon —
// un champ laissé vide CONSERVE la clé, il ne l'efface pas.

// sealedKey retourne la valeur à écrire dans la colonne api_key : la
// nouvelle clé scellée si l'opérateur en a saisi une, l'ancienne sinon —
// un champ laissé vide CONSERVE la clé, il ne l'efface pas.
func (h *Handlers) sealedKey(raw, existing string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return existing, nil
	}
	if h.LLMBox == nil {
		return "", fmt.Errorf("le secret de session ne permet pas de sceller une clé")
	}

	return h.LLMBox.Seal(strings.TrimSpace(raw))
}

// HandleLLMClientCreate enregistre un nouveau client.

// HandleLLMClientCreate enregistre un nouveau client.
func (h *Handlers) HandleLLMClientCreate(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	form := h.llmClientForm(r, true)
	form.CSRFToken = h.CSRFToken(w, r)

	fail := func(status int, message string) {
		form.Error = message
		h.Render(w, r, status, view.AdminLLMClientForm(form))
	}

	if !llmClientNamePattern.MatchString(form.Name) {
		fail(http.StatusBadRequest, "Le nom doit tenir en lettres minuscules, chiffres, tirets et soulignés (48 caractères au plus).")
		return
	}
	if message := validateLLMClientForm(form); message != "" {
		fail(http.StatusBadRequest, message)
		return
	}

	sealed, err := h.sealedKey(r.PostFormValue("api_key"), "")
	if err != nil {
		fail(http.StatusInternalServerError, "La clé n'a pas pu être chiffrée : "+err.Error())
		return
	}

	now := h.Now()
	var exists bool
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		_, found, err := h.LLMClients.Get(r.Context(), tx, form.Name)
		if err != nil {
			return err
		}
		if found {
			exists = true
			return nil
		}

		return h.LLMClients.Upsert(r.Context(), tx, persistence.LLMClient{
			Name:            form.Name,
			Kind:            form.Kind,
			Provider:        form.Provider,
			Model:           form.Model,
			BaseURL:         form.BaseURL,
			APIKey:          sealed,
			ReasoningEffort: form.ReasoningEffort,
			Vision:          form.Vision,
			ExtraFields:     form.ExtraFields,
			CreatedAt:       now,
			UpdatedAt:       now,
		})
	}) {
		return
	}
	if exists {
		fail(http.StatusConflict, "Un modèle porte déjà ce nom.")
		return
	}

	h.Logger.InfoContext(r.Context(), "web: modèle créé",
		"client", form.Name, "provider", form.Provider, "model", form.Model)

	http.Redirect(w, r, "/admin/llm-clients", http.StatusFound)
}

// HandleLLMClientForm affiche le formulaire d'édition. Le nom n'y est pas
// modifiable : c'est la clé à laquelle les rôles et les organisations se
// réfèrent. Renommer, c'est créer puis supprimer.

// HandleLLMClientForm affiche le formulaire d'édition. Le nom n'y est pas
// modifiable : c'est la clé à laquelle les rôles et les organisations se
// réfèrent. Renommer, c'est créer puis supprimer.
func (h *Handlers) HandleLLMClientForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var (
		row   persistence.LLMClient
		found bool
		uses  []persistence.OrgAgentClient
	)
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		if row, found, err = h.LLMClients.Get(r.Context(), tx, name); err != nil || !found {
			return err
		}
		uses, err = h.OrgClients.UsedBy(r.Context(), tx, name)
		return err
	}) {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	page := view.LLMClientFormPage{
		Platforms:       h.SidebarPlatforms(),
		CSRFToken:       h.CSRFToken(w, r),
		Name:            row.Name,
		Kind:            row.Kind,
		Provider:        row.Provider,
		Model:           row.Model,
		BaseURL:         row.BaseURL,
		ReasoningEffort: row.ReasoningEffort,
		Vision:          row.Vision,
		ExtraFields:     row.ExtraFields,
		HasKey:          row.APIKey != "",
		Saved:           r.URL.Query().Get("saved") != "",
		UsedBy:          h.llmClientUses(name, uses),
	}

	h.Render(w, r, http.StatusOK, view.AdminLLMClientForm(page))
}

// llmClientUses énumère ce qui retient un client : les rôles de
// l'instance (org_id vide) et les organisations qui l'ont choisi — tout
// vit dans la même table.

// llmClientUses énumère ce qui retient un client : les rôles de
// l'instance (org_id vide) et les organisations qui l'ont choisi — tout
// vit dans la même table.
func (h *Handlers) llmClientUses(name string, orgUses []persistence.OrgAgentClient) []string {
	var uses []string

	for _, use := range orgUses {
		if use.ClientName != name {
			continue
		}
		if use.OrgID == "" {
			uses = append(uses, "défaut de l'instance, rôle "+use.Role)
		} else {
			uses = append(uses, "organisation "+use.OrgID+", rôle "+use.Role)
		}
	}
	sort.Strings(uses)

	return uses
}

// HandleLLMClientUpdate enregistre les modifications d'un client existant.

// HandleLLMClientUpdate enregistre les modifications d'un client existant.
func (h *Handlers) HandleLLMClientUpdate(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")

	var (
		existing persistence.LLMClient
		found    bool
	)
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		existing, found, err = h.LLMClients.Get(r.Context(), tx, name)
		return err
	}) {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	form := h.llmClientForm(r, false)
	form.CSRFToken = h.CSRFToken(w, r)
	form.Name = name
	// Le nom et l'usage sont figés à la création : le formulaire ne les
	// poste pas, on garde ceux de la base.
	form.Kind = existing.Kind
	form.HasKey = existing.APIKey != ""

	fail := func(status int, message string) {
		form.Error = message
		form.UsedBy = h.llmClientUses(name, nil)
		h.Render(w, r, status, view.AdminLLMClientForm(form))
	}

	if message := validateLLMClientForm(form); message != "" {
		fail(http.StatusBadRequest, message)
		return
	}

	sealed, err := h.sealedKey(r.PostFormValue("api_key"), existing.APIKey)
	if err != nil {
		fail(http.StatusInternalServerError, "La clé n'a pas pu être chiffrée : "+err.Error())
		return
	}

	existing.Provider = form.Provider
	existing.Model = form.Model
	existing.BaseURL = form.BaseURL
	existing.APIKey = sealed
	existing.ReasoningEffort = form.ReasoningEffort
	existing.Vision = form.Vision
	existing.ExtraFields = form.ExtraFields
	existing.UpdatedAt = h.Now()

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.LLMClients.Upsert(r.Context(), tx, existing)
	}) {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: modèle modifié",
		"client", name, "provider", existing.Provider, "model", existing.Model)

	http.Redirect(w, r, "/admin/llm-clients/"+name+"?saved=1", http.StatusFound)
}

// roleLabels décrit les rôles système en français. Les autres rôles sont
// des noms d'agents, décrits par leur configuration.
var roleLabels = map[string][2]string{
	llmclients.RolePlugins: {
		"Sous-agents de plugins",
		"Le modèle qui pilote les outils des plugins (atelier, agenda, pages).",
	},
	llmclients.RolePluginsVision: {
		"Regard sur les images",
		"Le modèle multimodal derrière view_file, quand un sous-agent doit voir une image.",
	},
	llmclients.RoleJudge: {
		"Relecture des réponses sans outil",
		"Le modèle qui relit une réponse écrite sans qu'aucun outil ait été appelé, et dit si elle affirme un fait que rien n'appuie. Sans modèle affecté, aucune relecture n'a lieu.",
	},
	llmclients.RoleCompaction: {
		"Compaction de l'historique",
		"Le modèle qui condense les vieux messages en résumé roulant.",
	},
	llmclients.RoleTranscription: {
		"Transcription des notes vocales",
		"Le modèle qui transcrit l'audio entrant. Effet au redémarrage du service.",
	},
	llmclients.RoleConsolidation: {
		"Consolidation de la mémoire",
		"Le modèle de la passe nocturne qui fusionne et oublie les souvenirs. Effet au redémarrage.",
	},
	llmclients.RoleRetrieval: {
		"Recherche mémoire (HyDE)",
		"Le modèle qui reformule les requêtes de recherche mémoire. Effet au redémarrage.",
	},
}

// modelRoleRows dresse la liste des rôles réglables avec le catalogue
// propre à chacun : les modèles de conversation, ou ceux de génération
// d'images — les deux familles ne sont pas interchangeables.
//
// orgID vide dresse les rôles de l'INSTANCE : Chosen est alors le défaut
// lui-même, et il n'y a pas de niveau au-dessus (Default reste vide, un
// rôle sans choix est en alerte).

// HandleLLMClientDelete supprime un client, sauf si un rôle ou une
// organisation s'y réfère encore : la suppression laisserait alors des
// agents sans modèle.
func (h *Handlers) HandleLLMClientDelete(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")

	var uses []persistence.OrgAgentClient
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		uses, err = h.OrgClients.UsedBy(r.Context(), tx, name)
		return err
	}) {
		return
	}

	if held := h.llmClientUses(name, uses); len(held) > 0 {
		http.Error(w, "ce modèle est encore utilisé : "+strings.Join(held, ", "), http.StatusConflict)
		return
	}

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.LLMClients.Delete(r.Context(), tx, name)
	}) {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: modèle supprimé", "client", name)

	http.Redirect(w, r, "/admin/llm-clients", http.StatusFound)
}
