package web

import (
	"database/sql"
	"fmt"
	"net/http"

	"github.com/bornholm/automata/internal/web/view"
)

// handlePlatforms — ADM-05.
func (s *Server) handlePlatforms(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	pairing := r.URL.Query().Get("pairing")
	if pairing != "qr" && pairing != "credentials" {
		pairing = ""
	}

	page := view.PlatformsPage{
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
		Pairing:   pairing,
	}

	// Cartes de plateformes : go-courier n'expose pas d'état de connexion
	// interrogeable — l'état affiché reste « Configurée » (neutre) en
	// attendant l'instrumentation du lot suivant.
	channelCounts := map[string]int{}
	for _, ch := range s.cfg.Channels {
		channelCounts[ch.Provider]++
	}
	for name, provider := range s.cfg.Courier.Providers {
		if provider.Type == "mail" {
			// Le courriel sert aux codes de vérification, pas aux
			// conversations : il n'apparaît pas comme plateforme de canal.
			continue
		}
		count := channelCounts[name]
		label := "canaux configurés"
		if count == 1 {
			label = "canal configuré"
		}
		page.Cards = append(page.Cards, view.PlatformCard{
			Type: provider.Type,
			Name: platformDisplayName(provider.Type, name),
			Chip: view.Chip{Label: "Configurée", Tone: "neutral", Dot: true},
			Details: []string{
				fmt.Sprintf("%d %s", count, label),
				"État de connexion : prochain lot",
			},
		})
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		// Canaux actifs de la configuration.
		for _, ch := range s.cfg.Channels {
			page.Channels = append(page.Channels, view.ChannelRow{
				PlatformType: providerTypeOf(s.cfg, ch.Provider),
				Name:         ch.DisplayName,
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
				TokenPrefix: TokenPrefix(token.ID),
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
