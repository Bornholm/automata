package web

import (
	"context"
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

// handleLLMClients liste le catalogue.
func (s *Server) handleLLMClients(w http.ResponseWriter, r *http.Request) {
	page := view.LLMClientsPage{
		Platforms: s.SidebarPlatforms(),
		CSRFToken: s.CSRFToken(w, r),
	}

	if s.LLMBox == nil {
		page.Error = "Le catalogue est en lecture seule : le secret de session ne permet pas d'ouvrir les clés enregistrées."
	}

	var rows []persistence.LLMClient
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		rows, err = s.LLMClients.List(r.Context(), tx, "")
		return err
	}) {
		return
	}

	// Les rôles servis par défaut se lisent en base (org_id vide) : ce
	// sont eux qui disent à quoi sert réellement chaque entrée du
	// catalogue — le fichier de configuration ne les connaît plus.
	var instanceDefaults map[string]string
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		instanceDefaults, err = s.OrgClients.ListByOrg(r.Context(), tx, "")
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
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		page.Roles, err = s.modelRoleRows(r.Context(), tx, "")
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

	s.Render(w, r, http.StatusOK, view.AdminLLMClients(page))
}

// handleLLMClientNewForm affiche le formulaire de création.
func (s *Server) handleLLMClientNewForm(w http.ResponseWriter, r *http.Request) {
	s.Render(w, r, http.StatusOK, view.AdminLLMClientForm(view.LLMClientFormPage{
		Platforms: s.SidebarPlatforms(),
		CSRFToken: s.CSRFToken(w, r),
		New:       true,
		Kind:      persistence.LLMClientKindLLM,
		Vision:    true,
	}))
}

// llmClientForm lit le formulaire commun à la création et à l'édition.
func (s *Server) llmClientForm(r *http.Request, isNew bool) view.LLMClientFormPage {
	kind := strings.TrimSpace(r.PostFormValue("kind"))
	if kind != persistence.LLMClientKindImage {
		kind = persistence.LLMClientKindLLM
	}

	return view.LLMClientFormPage{
		Platforms:       s.SidebarPlatforms(),
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
func (s *Server) sealedKey(raw, existing string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return existing, nil
	}
	if s.LLMBox == nil {
		return "", fmt.Errorf("le secret de session ne permet pas de sceller une clé")
	}

	return s.LLMBox.Seal(strings.TrimSpace(raw))
}

// handleLLMClientCreate enregistre un nouveau client.
func (s *Server) handleLLMClientCreate(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	form := s.llmClientForm(r, true)
	form.CSRFToken = s.CSRFToken(w, r)

	fail := func(status int, message string) {
		form.Error = message
		s.Render(w, r, status, view.AdminLLMClientForm(form))
	}

	if !llmClientNamePattern.MatchString(form.Name) {
		fail(http.StatusBadRequest, "Le nom doit tenir en lettres minuscules, chiffres, tirets et soulignés (48 caractères au plus).")
		return
	}
	if message := validateLLMClientForm(form); message != "" {
		fail(http.StatusBadRequest, message)
		return
	}

	sealed, err := s.sealedKey(r.PostFormValue("api_key"), "")
	if err != nil {
		fail(http.StatusInternalServerError, "La clé n'a pas pu être chiffrée : "+err.Error())
		return
	}

	now := s.Now()
	var exists bool
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		_, found, err := s.LLMClients.Get(r.Context(), tx, form.Name)
		if err != nil {
			return err
		}
		if found {
			exists = true
			return nil
		}

		return s.LLMClients.Upsert(r.Context(), tx, persistence.LLMClient{
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

	s.Logger.InfoContext(r.Context(), "web: modèle créé",
		"client", form.Name, "provider", form.Provider, "model", form.Model)

	http.Redirect(w, r, "/admin/llm-clients", http.StatusFound)
}

// handleLLMClientForm affiche le formulaire d'édition. Le nom n'y est pas
// modifiable : c'est la clé à laquelle les rôles et les organisations se
// réfèrent. Renommer, c'est créer puis supprimer.
func (s *Server) handleLLMClientForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var (
		row   persistence.LLMClient
		found bool
		uses  []persistence.OrgAgentClient
	)
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		if row, found, err = s.LLMClients.Get(r.Context(), tx, name); err != nil || !found {
			return err
		}
		uses, err = s.OrgClients.UsedBy(r.Context(), tx, name)
		return err
	}) {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	page := view.LLMClientFormPage{
		Platforms:       s.SidebarPlatforms(),
		CSRFToken:       s.CSRFToken(w, r),
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
		UsedBy:          s.llmClientUses(name, uses),
	}

	s.Render(w, r, http.StatusOK, view.AdminLLMClientForm(page))
}

// llmClientUses énumère ce qui retient un client : les rôles de
// l'instance (org_id vide) et les organisations qui l'ont choisi — tout
// vit dans la même table.
func (s *Server) llmClientUses(name string, orgUses []persistence.OrgAgentClient) []string {
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

// handleLLMClientUpdate enregistre les modifications d'un client existant.
func (s *Server) handleLLMClientUpdate(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")

	var (
		existing persistence.LLMClient
		found    bool
	)
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		existing, found, err = s.LLMClients.Get(r.Context(), tx, name)
		return err
	}) {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	form := s.llmClientForm(r, false)
	form.CSRFToken = s.CSRFToken(w, r)
	form.Name = name
	// Le nom et l'usage sont figés à la création : le formulaire ne les
	// poste pas, on garde ceux de la base.
	form.Kind = existing.Kind
	form.HasKey = existing.APIKey != ""

	fail := func(status int, message string) {
		form.Error = message
		form.UsedBy = s.llmClientUses(name, nil)
		s.Render(w, r, status, view.AdminLLMClientForm(form))
	}

	if message := validateLLMClientForm(form); message != "" {
		fail(http.StatusBadRequest, message)
		return
	}

	sealed, err := s.sealedKey(r.PostFormValue("api_key"), existing.APIKey)
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
	existing.UpdatedAt = s.Now()

	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		return s.LLMClients.Upsert(r.Context(), tx, existing)
	}) {
		return
	}

	s.Logger.InfoContext(r.Context(), "web: modèle modifié",
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
func (s *Server) modelRoleRows(ctx context.Context, tx *sql.Tx, orgID string) ([]view.OrgModelRole, error) {
	catalog, err := s.LLMClients.List(ctx, tx, "")
	if err != nil {
		return nil, err
	}

	chosen, err := s.OrgClients.ListByOrg(ctx, tx, orgID)
	if err != nil {
		return nil, err
	}

	// Les défauts d'instance donnent, sur l'écran d'une organisation, le
	// libellé de l'option « défaut » ; sur l'écran de l'instance ils SONT
	// le choix.
	instanceDefaults := chosen
	if orgID != "" {
		if instanceDefaults, err = s.OrgClients.ListByOrg(ctx, tx, ""); err != nil {
			return nil, err
		}
	}

	models := make(map[string]string, len(catalog))
	byKind := map[string][]view.OrgModelOption{}
	for _, row := range catalog {
		models[row.Name] = row.Model
		byKind[row.Kind] = append(byKind[row.Kind], view.OrgModelOption{Name: row.Name, Model: row.Model})
	}

	roleNames := llmclients.Roles(s.Cfg)
	roles := make([]view.OrgModelRole, 0, len(roleNames))
	for _, role := range roleNames {
		entry := view.OrgModelRole{
			Role:         role,
			Chosen:       chosen[role],
			Options:      byKind[persistence.LLMClientKindLLM],
			InstanceView: orgID == "",
		}
		if orgID != "" {
			entry.Default = instanceDefaults[role]
			entry.DefaultModel = models[entry.Default]
		}

		switch {
		case strings.HasPrefix(role, llmclients.RoleImagePrefix):
			agentName := strings.TrimPrefix(role, llmclients.RoleImagePrefix)
			entry.Label = "Images de l'agent " + agentName
			entry.Hint = "Le modèle qui dessine derrière generate_image, distinct de celui qui converse."
			entry.Options = byKind[persistence.LLMClientKindImage]
		case strings.HasPrefix(role, llmclients.RoleEmbeddingsPrefix):
			indexID := strings.TrimPrefix(role, llmclients.RoleEmbeddingsPrefix)
			entry.Label = "Embeddings de l'index " + indexID
			entry.Hint = "VERROUILLÉ après le premier démarrage : changer de modèle rendrait les vecteurs déjà écrits incomparables. Effet au redémarrage."
		case roleLabels[role] != [2]string{}:
			labels := roleLabels[role]
			entry.Label, entry.Hint = labels[0], labels[1]
		default:
			entry.Label = "Agent " + role
			entry.Hint = s.Cfg.Agents[role].Description
			if entry.Hint == "" {
				entry.Hint = "Agent déclaré dans la configuration de l'instance."
			}
		}

		roles = append(roles, entry)
	}

	// Les agents d'abord, par ordre alphabétique, puis les rôles système :
	// l'opérateur cherche d'abord son assistant, pas la compaction.
	sort.Slice(roles, func(i, j int) bool {
		iSystem, jSystem := roleLabels[roles[i].Role], roleLabels[roles[j].Role]
		if (iSystem != [2]string{}) != (jSystem != [2]string{}) {
			return jSystem != [2]string{}
		}
		return roles[i].Role < roles[j].Role
	})

	return roles, nil
}

// handleOrgModels enregistre les modèles choisis par une organisation. Un
// champ laissé sur « défaut de l'instance » efface la surcharge : une
// organisation ne garde jamais un choix qu'elle n'a plus exprimé.
func (s *Server) handleOrgModels(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	s.saveModelRoles(w, r, r.PathValue("id"), "/admin/orgs/"+r.PathValue("id")+"?tab=models")
}

// handleInstanceModels enregistre les défauts d'instance : mêmes règles que
// pour une organisation, org_id vide.
func (s *Server) handleInstanceModels(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	s.saveModelRoles(w, r, "", "/admin/llm-clients")
}

// saveModelRoles enregistre les choix de modèles postés, pour une
// organisation ou pour l'instance (orgID vide). Un champ laissé vide efface
// la ligne : personne ne garde un choix qu'il n'a plus exprimé.
func (s *Server) saveModelRoles(w http.ResponseWriter, r *http.Request, orgID, redirect string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "formulaire illisible", http.StatusBadRequest)
		return
	}

	now := s.Now()
	var unknown string

	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		catalog, err := s.LLMClients.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		kinds := make(map[string]string, len(catalog))
		for _, row := range catalog {
			kinds[row.Name] = row.Kind
		}

		// Seuls les rôles que l'instance déclare sont acceptés : un champ
		// forgé ne doit pas créer une ligne que rien ne lira. Et chaque
		// rôle n'accepte que sa famille — un générateur d'images ne
		// conversera jamais.
		for _, role := range llmclients.Roles(s.Cfg) {
			name := strings.TrimSpace(r.PostFormValue("role:" + role))

			if name == "" {
				if err := s.OrgClients.Unset(r.Context(), tx, orgID, role); err != nil {
					return err
				}
				continue
			}

			wantKind := persistence.LLMClientKindLLM
			if strings.HasPrefix(role, llmclients.RoleImagePrefix) {
				wantKind = persistence.LLMClientKindImage
			}
			if kinds[name] != wantKind {
				unknown = name
				return nil
			}

			if err := s.OrgClients.Set(r.Context(), tx, persistence.OrgAgentClient{
				OrgID:      orgID,
				Role:       role,
				ClientName: name,
				UpdatedAt:  now,
			}); err != nil {
				return err
			}
		}

		return nil
	}) {
		return
	}

	if unknown != "" {
		http.Error(w, "modèle inconnu ou de la mauvaise famille : "+unknown, http.StatusBadRequest)
		return
	}

	s.Logger.InfoContext(r.Context(), "web: rôles de modèles modifiés", "org", orgID)

	http.Redirect(w, r, redirect, http.StatusFound)
}

// handleLLMClientDelete supprime un client, sauf si un rôle ou une
// organisation s'y réfère encore : la suppression laisserait alors des
// agents sans modèle.
func (s *Server) handleLLMClientDelete(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")

	var uses []persistence.OrgAgentClient
	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		uses, err = s.OrgClients.UsedBy(r.Context(), tx, name)
		return err
	}) {
		return
	}

	if held := s.llmClientUses(name, uses); len(held) > 0 {
		http.Error(w, "ce modèle est encore utilisé : "+strings.Join(held, ", "), http.StatusConflict)
		return
	}

	if !s.WithTx(w, r, func(tx *sql.Tx) error {
		return s.LLMClients.Delete(r.Context(), tx, name)
	}) {
		return
	}

	s.Logger.InfoContext(r.Context(), "web: modèle supprimé", "client", name)

	http.Redirect(w, r, "/admin/llm-clients", http.StatusFound)
}
