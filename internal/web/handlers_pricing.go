package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// handlePricing — ADM-08.
func (s *Server) handlePricing(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	from, to := monthBounds(now)

	page := view.PricingPage{
		Platforms: s.sidebarPlatforms(),
		CSRFToken: s.csrfToken(w, r),
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		p, err := s.pricing(r.Context(), tx)
		if err != nil {
			return err
		}

		m, err := s.computeMargin(r.Context(), tx, p, from, to)
		if err != nil {
			return err
		}

		page.USDPerCredit = trimFloat(p.USDPerCredit)
		page.EURPerUSD = trimFloat(p.EURPerUSD)
		page.WelcomeCredits = p.WelcomeCredits
		page.DefaultAllowance = p.DefaultAllowance

		page.Margin = view.MarginPanel{
			SoldCredits:  m.SoldCredits,
			SoldEUR:      formatEuros(m.SoldEUR),
			GivenCredits: m.GivenCredits,
			UsedCredits:  m.UsedCredits,
			CostUSD:      fmt.Sprintf("%.2f $", m.CostUSD),
			CostEUR:      formatEuros(m.CostEUR),
			MarginEUR:    formatEuros(m.MarginEUR),
			Positive:     m.MarginEUR >= 0,
			Unreported:   m.Calls - m.ReportedCalls,
			Period:       strings.ToLower(view.FormatMonth(now)) + " " + strconv.Itoa(now.Year()),
		}
		if m.SoldEUR > 0 {
			page.Margin.Ratio = fmt.Sprintf("%.0f %% des recettes", m.MarginEUR/m.SoldEUR*100)
		} else {
			page.Margin.Ratio = "aucune recette sur la période"
		}

		for _, pack := range p.Packs {
			row := view.PricingPack{
				ID:         pack.ID,
				Credits:    pack.Credits,
				PriceEUR:   formatEuros(pack.PriceEUR),
				Featured:   pack.Featured,
				FromConfig: pack.ID < 0,
			}
			if pack.Credits > 0 {
				row.PerThousand = formatEuros(pack.PriceEUR / float64(pack.Credits) * 1000)
			}
			page.Packs = append(page.Packs, row)
		}

		return nil
	})
	if !ok {
		return
	}

	if r.URL.Query().Get("saved") == "1" {
		page.Flash = "Réglages enregistrés. Ils s'appliquent aux prochains débits et aux pages de crédits."
	}

	s.render(w, r, http.StatusOK, view.AdminPricing(page))
}

// trimFloat rend un nombre sans zéros inutiles, pour un champ de saisie.
func trimFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// handlePricingPackCreate ajoute une offre.
func (s *Server) handlePricingPackCreate(w http.ResponseWriter, r *http.Request) {
	credits, errCredits := strconv.ParseInt(r.PostFormValue("credits"), 10, 64)
	price, errPrice := strconv.ParseFloat(strings.Replace(r.PostFormValue("price_eur"), ",", ".", 1), 64)

	if errCredits != nil || errPrice != nil || credits <= 0 || price < 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		packs, err := s.pricingRepo.ListPacks(r.Context(), tx)
		if err != nil {
			return err
		}

		return s.pricingRepo.InsertPack(r.Context(), tx, persistence.CreditPack{
			Credits:   credits,
			PriceEUR:  price,
			Position:  len(packs),
			CreatedAt: s.now(),
		})
	})
	if !ok {
		return
	}

	s.logger.InfoContext(r.Context(), "web: offre de crédits ajoutée", "credits", credits)
	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// handlePricingPackDelete retire une offre.
func (s *Server) handlePricingPackDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		return s.pricingRepo.DeletePack(r.Context(), tx, id)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// handlePricingPackFeature met une offre en avant, une seule à la fois.
func (s *Server) handlePricingPackFeature(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		if err := s.pricingRepo.ClearFeatured(r.Context(), tx); err != nil {
			return err
		}
		return s.pricingRepo.SetFeatured(r.Context(), tx, id)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// handlePricingSettings enregistre les réglages de conversion.
func (s *Server) handlePricingSettings(w http.ResponseWriter, r *http.Request) {
	now := s.now()

	ok := s.withTx(w, r, func(tx *sql.Tx) error {
		for key, raw := range map[string]string{
			persistence.SettingUSDPerCredit:     r.PostFormValue("usd_per_credit"),
			persistence.SettingEURPerUSD:        r.PostFormValue("eur_per_usd"),
			persistence.SettingWelcomeCredits:   r.PostFormValue("welcome_credits"),
			persistence.SettingDefaultAllowance: r.PostFormValue("default_allowance"),
		} {
			value := strings.TrimSpace(strings.Replace(raw, ",", ".", 1))
			if value == "" {
				continue
			}
			// Un réglage illisible est ignoré plutôt qu'écrit : mieux vaut
			// conserver l'ancien que casser la conversion des débits.
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				continue
			}
			if err := s.pricingRepo.SetSetting(r.Context(), tx, key, value, now); err != nil {
				return err
			}
		}

		return nil
	})
	if !ok {
		return
	}

	s.logger.InfoContext(r.Context(), "web: réglages de tarification enregistrés")
	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}
