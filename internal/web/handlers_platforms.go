package web

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/platform"

	"github.com/bornholm/automata/internal/web/view"
	"github.com/bornholm/automata/internal/weblink"
)

// handlePlatforms — ADM-05.
func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	pairing := r.URL.Query().Get("pairing")
	if pairing != "qr" && pairing != "credentials" {
		pairing = ""
	}

	// Plateforme sélectionnée dans le gabarit d'appairage : elle décide de
	// la variante affichée autant que du surlignage du sélecteur.
	pairingPlatform := r.URL.Query().Get("platform")
	switch pairingPlatform {
	case "whatsapp", "signal":
		pairing = "qr"
	case "rocket":
		pairing = "credentials"
	default:
		if pairing == "credentials" {
			pairingPlatform = "rocket"
		} else if pairing == "qr" {
			pairingPlatform = "whatsapp"
		}
	}

	page := view.PlatformsPage{
		Platforms:       s.sidebarPlatforms(),
		CSRFToken:       s.csrfToken(w, r),
		Pairing:         pairing,
		PairingPlatform: pairingPlatform,
	}

	for _, field := range platformFields(pairingPlatform) {
		page.Fields = append(page.Fields, view.PlatformField{
			Name:        field.Name,
			Label:       field.Label,
			Placeholder: field.Placeholder,
			Secret:      field.Secret,
			Required:    field.Required,
		})
	}

	switch r.URL.Query().Get("error") {
	case "champs":
		page.Error = "Tous les champs requis doivent être renseignés."
	case "type":
		page.Error = "Ce type de compte n'est pas supporté."
	case "invalide":
		page.Error = "Cette configuration ne permet pas de joindre le service : vérifiez l'adresse et les identifiants."
	}

	// Cartes de plateformes : les comptes enregistrés, avec leur état réel
	// tel que le gestionnaire l'observe (internal/platform).
	statuses := map[string]platform.Status{}
	if s.platformManager != nil {
		statuses = s.platformManager.Statuses()
	}

	// Canaux d'un compte de messagerie : ceux déclarés en configuration
	// et ceux rattachés en ligne par jeton. Oublier les seconds affichait
	// « 0 canaux » sur la carte d'une plateforme dont la table, juste en
	// dessous, en listait trois.
	configChannelCounts := map[string]int{}
	for _, ch := range s.cfg.Channels {
		configChannelCounts[ch.Provider]++
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		accounts, err := s.platforms.List(r.Context(), tx)
		if err != nil {
			return err
		}

		// Canaux liés dynamiquement (jetons de groupe consommés) : lus
		// avant les cartes, qui en affichent le compte.
		bindings, err := s.bindings.ListAll(r.Context(), tx)
		if err != nil {
			return err
		}

		channelCounts := map[string]int{}
		for provider, count := range configChannelCounts {
			channelCounts[provider] += count
		}
		for _, binding := range bindings {
			channelCounts[binding.Provider]++
		}

		for _, account := range accounts {
			status := statuses[account.ID]
			state := string(status.State)
			if !account.Enabled {
				state = "stopped"
			}
			label, tone := platformStatusChip(state)

			name := account.DisplayName
			if name == "" {
				name = platformTypeLabel(account.Type)
			}

			count := channelCounts[account.ID]
			countLabel := "canaux"
			if count == 1 {
				countLabel = "canal"
			}

			details := []string{fmt.Sprintf("%d %s", count, countLabel)}
			switch {
			case status.Err != "":
				details = append(details, status.Err)
			case status.Since.IsZero():
				details = append(details, "jamais démarrée")
			default:
				details = append(details, sinceLabel(status.Since))
			}

			card := view.PlatformCard{
				ID:      account.ID,
				Type:    account.Type,
				Name:    name,
				Chip:    view.Chip{Label: label, Tone: tone, Dot: true},
				Details: details,
				Enabled: account.Enabled,
			}

			// Un compte en attente d'appairage affiche son QR : c'est ce qui
			// remplace le code imprimé dans les journaux du worker.
			if status.Pairing() {
				if uri, err := qrPNGDataURI(status.PairingCode); err == nil {
					card.PairingQR = uri
				}
			}

			page.Cards = append(page.Cards, card)
		}

		for _, binding := range bindings {
			page.Channels = append(page.Channels, view.ChannelRow{
				PlatformType: platformTypeOf(accounts, binding.Provider),
				Name:         binding.DisplayName,
				Kind:         channelKindLabelFromScope(binding.Kind),
				OrgName:      s.orgDisplayName(r.Context(), tx, binding.OrgID),
				Chip:         view.Chip{Label: "Actif", Tone: "ok"},
			})
			page.Active++
		}

		// Canaux actifs de la configuration.
		for _, ch := range s.cfg.Channels {
			page.Channels = append(page.Channels, view.ChannelRow{
				PlatformType: s.providerTypeOf(ch.Provider),
				Name:         s.channelDisplayName(ch),
				Kind:         channelKindLabel(ch.Kind),
				OrgName:      s.orgDisplayName(r.Context(), tx, ch.OrgID),
				Chip:         view.Chip{Label: "Actif", Tone: "ok"},
			})
			page.Active++
		}

		// Jetons de groupe en attente : les futurs canaux.
		pending, err := s.linkTokens.ListPendingGroup(r.Context(), tx)
		if err != nil {
			return err
		}
		for _, token := range pending {
			if token.Expired(now) {
				continue
			}
			page.Channels = append(page.Channels, view.ChannelRow{
				Name:        "Canal à lier",
				Kind:        "Groupe",
				OrgName:     s.orgDisplayName(r.Context(), tx, token.OrgID),
				Chip:        view.Chip{Label: "En attente de liaison", Tone: "warn"},
				TokenPrefix: weblink.TokenPrefix(token.ID),
				RowTone:     "warn",
			})
			page.PendingCnt++
		}

		orgs, err := s.orgs.List(r.Context(), tx, "")
		if err != nil {
			return err
		}
		for _, org := range orgs {
			page.Orgs = append(page.Orgs, struct{ ID, Name string }{org.ID, org.DisplayName})
		}

		return nil
	})
	if !ok {
		return
	}

	if key := r.URL.Query().Get("reveal"); key != "" {
		if value, ok := s.reveals.pop(key, now); ok {
			page.FlashToken = &view.TokenPanelData{
				Eyebrow:   "Jeton de groupe",
				Display:   value.Display,
				Clipboard: value.Clear,
				Help:      "Envoyez ce code dans la conversation de groupe à rattacher : Automata liera le canal à l'organisation choisie. Après fermeture de cette page, le code ne pourra plus être affiché.",
				Status:    "pending",
			}
		}
	}

	s.render(w, r, http.StatusOK, view.AdminPlatforms(page))
}

func (s *Server) handlePlatformsGroupToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PostFormValue("org_id")
	if orgID == "" {
		http.Redirect(w, r, "/admin/platforms", http.StatusFound)
		return
	}
	s.createGroupToken(w, r, orgID, "/admin/platforms")
}

// platformTypeOf retrouve le type d'un compte enregistré.
func platformTypeOf(accounts []persistence.Platform, id string) string {
	for _, account := range accounts {
		if account.ID == id {
			return account.Type
		}
	}
	return ""
}

// channelKindLabelFromScope traduit le genre d'un canal lié dynamiquement.
func channelKindLabelFromScope(kind string) string {
	if kind == "group" {
		return "Groupe"
	}
	return "Privé"
}

// platformTypeLabel nomme un type de compte pour l'affichage.
func platformTypeLabel(providerType string) string {
	return platformDisplayName(providerType, providerType)
}
