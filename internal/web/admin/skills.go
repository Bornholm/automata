package admin

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/skills"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// Administration de la bibliothèque de compétences (voir docs/skills.md).
// Le contenu d'une compétence part au modèle : la vue le rappelle, et le
// serveur ne s'en mêle pas — un opérateur écrit ce qu'il veut, en
// anglais de préférence.
//
// Journaux : nom de la compétence et action, jamais le contenu.

// parseAgents lit le champ de ciblage (noms d'agents séparés par des
// virgules). Vide = visible de tous les agents.
func parseAgents(raw string) []string {
	var agents []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			agents = append(agents, part)
		}
	}
	return agents
}

// HandleSkills liste la bibliothèque.
func (h *Handlers) HandleSkills(w http.ResponseWriter, r *http.Request) {
	page := view.SkillsPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}

	var rows []persistence.Skill
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		rows, err = h.Skills.List(r.Context(), tx)
		return err
	}) {
		return
	}

	for _, skill := range rows {
		page.Skills = append(page.Skills, view.SkillRow{
			Name:        skill.Name,
			Description: skill.Description,
			Agents:      strings.Join(skill.Agents, ", "),
			Enabled:     skill.Enabled,
			Builtin:     skill.Builtin,
			UpdatedAt:   view.FormatShortDate(skill.UpdatedAt),
		})
	}

	h.Render(w, r, http.StatusOK, view.AdminSkills(page))
}

// HandleSkillNewForm affiche le formulaire de création.
func (h *Handlers) HandleSkillNewForm(w http.ResponseWriter, r *http.Request) {
	h.Render(w, r, http.StatusOK, view.AdminSkillForm(view.SkillFormPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
		New:       true,
	}))
}

// HandleSkillCreate enregistre une nouvelle compétence.
func (h *Handlers) HandleSkillCreate(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	form := view.SkillFormPage{
		Platforms:   h.SidebarPlatforms(),
		CSRFToken:   h.CSRFToken(w, r),
		New:         true,
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Content:     strings.TrimSpace(r.PostFormValue("content")),
		Agents:      strings.TrimSpace(r.PostFormValue("agents")),
		Enabled:     r.PostFormValue("enabled") != "",
	}

	fail := func(status int, message string) {
		form.Error = message
		h.Render(w, r, status, view.AdminSkillForm(form))
	}

	if !skills.ValidName(form.Name) {
		fail(http.StatusBadRequest, "Le nom doit être en kebab-case : lettres minuscules, chiffres et tirets.")
		return
	}
	if form.Description == "" || form.Content == "" {
		fail(http.StatusBadRequest, "La description et le contenu sont requis.")
		return
	}

	now := h.Now()
	var exists bool
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		_, found, err := h.Skills.Get(r.Context(), tx, form.Name)
		if err != nil {
			return err
		}
		if found {
			exists = true
			return nil
		}

		return h.Skills.Upsert(r.Context(), tx, persistence.Skill{
			Name:        form.Name,
			Description: form.Description,
			Content:     form.Content,
			Agents:      parseAgents(form.Agents),
			Enabled:     form.Enabled,
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	}) {
		return
	}
	if exists {
		fail(http.StatusConflict, "Une compétence porte déjà ce nom.")
		return
	}

	h.Logger.InfoContext(r.Context(), "web: compétence créée", "skill", form.Name)

	http.Redirect(w, r, "/admin/skills", http.StatusFound)
}

// HandleSkillForm affiche le formulaire d'édition. Le nom n'y est pas
// modifiable : c'est la clé primaire, et le ciblage des compétences s'y
// réfère. Renommer, c'est créer puis supprimer.
func (h *Handlers) HandleSkillForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	var (
		skill persistence.Skill
		found bool
	)
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		var err error
		skill, found, err = h.Skills.Get(r.Context(), tx, name)
		return err
	}) {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	h.Render(w, r, http.StatusOK, view.AdminSkillForm(view.SkillFormPage{
		Platforms:   h.SidebarPlatforms(),
		CSRFToken:   h.CSRFToken(w, r),
		Name:        skill.Name,
		Description: skill.Description,
		Content:     skill.Content,
		Agents:      strings.Join(skill.Agents, ", "),
		Enabled:     skill.Enabled,
		Builtin:     skill.Builtin,
		Edited:      skill.Edited,
		Saved:       r.URL.Query().Get("saved") != "",
	}))
}

// HandleSkillUpdate enregistre l'édition d'une compétence.
func (h *Handlers) HandleSkillUpdate(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	description := strings.TrimSpace(r.PostFormValue("description"))
	content := strings.TrimSpace(r.PostFormValue("content"))
	agents := parseAgents(r.PostFormValue("agents"))
	enabled := r.PostFormValue("enabled") != ""

	var (
		found   bool
		builtin bool
	)
	if description == "" || content == "" {
		if !h.WithTx(w, r, func(tx *sql.Tx) error {
			existing, ok, err := h.Skills.Get(r.Context(), tx, name)
			found, builtin = ok, existing.Builtin
			return err
		}) {
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}

		h.Render(w, r, http.StatusBadRequest, view.AdminSkillForm(view.SkillFormPage{
			Platforms:   h.SidebarPlatforms(),
			CSRFToken:   h.CSRFToken(w, r),
			Name:        name,
			Description: description,
			Content:     content,
			Agents:      strings.Join(agents, ", "),
			Enabled:     enabled,
			Builtin:     builtin,
			Error:       "La description et le contenu sont requis.",
		}))
		return
	}

	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		existing, ok, err := h.Skills.Get(r.Context(), tx, name)
		if err != nil || !ok {
			return err
		}
		found = true

		existing.Description = description
		existing.Content = content
		existing.Agents = agents
		existing.Enabled = enabled
		// Marquée éditée : le semis ne la remplacera plus par le contenu
		// embarqué au prochain démarrage. « Restaurer » lève ce marqueur.
		existing.Edited = true
		existing.UpdatedAt = h.Now()

		return h.Skills.Upsert(r.Context(), tx, existing)
	}) {
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	h.Logger.InfoContext(r.Context(), "web: compétence modifiée", "skill", name, "enabled", enabled)

	http.Redirect(w, r, "/admin/skills/"+name+"?saved=1", http.StatusFound)
}

// HandleSkillDelete supprime une compétence. Une compétence fournie par
// le projet sera re-semée au prochain démarrage : la vue le dit.
func (h *Handlers) HandleSkillDelete(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.Skills.Delete(r.Context(), tx, name)
	}) {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: compétence supprimée", "skill", name)

	http.Redirect(w, r, "/admin/skills", http.StatusFound)
}

// HandleSkillRestore réécrit une compétence fournie par le projet depuis
// sa version embarquée. Sans effet sur une compétence écrite à la main :
// le dépôt n'en a aucune version d'origine.
func (h *Handlers) HandleSkillRestore(w http.ResponseWriter, r *http.Request) {
	if !core.CheckCSRF(r) {
		http.Error(w, "jeton CSRF absent ou invalide", http.StatusForbidden)
		return
	}

	name := r.PathValue("name")
	def, ok := skills.BuiltinContent(name)
	if !ok {
		http.NotFound(w, r)
		return
	}

	now := h.Now()
	if !h.WithTx(w, r, func(tx *sql.Tx) error {
		existing, found, err := h.Skills.Get(r.Context(), tx, name)
		if err != nil {
			return err
		}
		if !found {
			existing = persistence.Skill{Name: name, Enabled: true, CreatedAt: now}
		}

		existing.Description = def.Description
		existing.Content = def.Content
		existing.Agents = def.Agents
		existing.Builtin = true
		// Restaurée : elle redevient suiveuse des mises à jour du dépôt.
		existing.Edited = false
		existing.UpdatedAt = now

		return h.Skills.Upsert(r.Context(), tx, existing)
	}) {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: compétence restaurée", "skill", name)

	http.Redirect(w, r, "/admin/skills/"+name+"?saved=1", http.StatusFound)
}
