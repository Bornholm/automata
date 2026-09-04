package admin

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/core"
	"github.com/bornholm/automata/internal/web/view"
)

// HandlePricing — ADM-08.
func (h *Handlers) HandlePricing(w http.ResponseWriter, r *http.Request) {
	now := h.Now()
	from, to := core.MonthBounds(now)

	page := view.PricingPage{
		Platforms: h.SidebarPlatforms(),
		CSRFToken: h.CSRFToken(w, r),
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		p, err := h.Pricing(r.Context(), tx)
		if err != nil {
			return err
		}

		m, err := h.ComputeMargin(r.Context(), tx, p, from, to)
		if err != nil {
			return err
		}

		page.USDPerCredit = trimFloat(p.USDPerCredit)
		page.TargetMargin = trimFloat(p.TargetMargin)
		page.CreditCost = fmt.Sprintf("%.5f €", p.CreditCostEUR())
		// Valeurs brutes pour l'aperçu côté navigateur : le texte formaté
		// ci-dessus n'est pas relisible par un calcul.
		page.CreditCostRaw = strconv.FormatFloat(p.CreditCostEUR(), 'f', -1, 64)
		page.TargetMarginRaw = strconv.FormatFloat(p.TargetMargin, 'f', -1, 64)
		page.EURPerUSD = trimFloat(p.EURPerUSD)
		page.WelcomeCredits = p.WelcomeCredits
		page.DefaultAllowance = p.DefaultAllowance

		page.Margin = view.MarginPanel{
			SoldCredits:  m.SoldCredits,
			SoldEUR:      core.FormatEuros(m.SoldEUR),
			GivenCredits: m.GivenCredits,
			UsedCredits:  m.UsedCredits,
			CostUSD:      fmt.Sprintf("%.2f $", m.CostUSD),
			CostEUR:      core.FormatEuros(m.CostEUR),
			MarginEUR:    core.FormatEuros(m.MarginEUR),
			Positive:     m.MarginEUR >= 0,
			Unreported:   m.Calls - m.ReportedCalls,
			Period:       strings.ToLower(view.FormatMonth(r.Context(), now)) + " " + strconv.Itoa(now.Year()),
		}
		if m.SoldEUR > 0 {
			page.Margin.Ratio = fmt.Sprintf("%.0f %% des recettes", m.MarginEUR/m.SoldEUR*100)
		} else {
			page.Margin.Ratio = "aucune recette sur la période"
		}

		prices, err := h.ModelPrices.List(r.Context(), tx)
		if err != nil {
			return err
		}
		for _, price := range prices {
			page.ModelPrices = append(page.ModelPrices, view.ModelPriceRow{
				Model:  price.Model,
				Input:  trimFloat(price.InputPerMillion),
				Output: trimFloat(price.OutputPerMillion),
			})
		}
		page.DefaultInput = trimFloat(p.DefaultInput)
		page.DefaultOutput = trimFloat(p.DefaultOutput)

		if m.Calls > 0 {
			estimated := m.Calls - m.ReportedCalls
			page.EstimatedShare = fmt.Sprintf("%d appel(s) sur %d estimés faute de coût rapporté", estimated, m.Calls)
		} else {
			page.EstimatedShare = "aucun appel sur la période"
		}

		for _, pack := range p.Packs {
			row := view.PricingPack{
				ID:         pack.ID,
				Credits:    pack.Credits,
				PriceEUR:   core.FormatEuros(pack.PriceEUR),
				Featured:   pack.Featured,
				FromConfig: pack.ID < 0,
			}
			if pack.Credits > 0 {
				row.PerThousand = core.FormatEuros(pack.PriceEUR / float64(pack.Credits) * 1000)
			}

			// Marge de l'offre : c'est le seul endroit où l'on voit, avant
			// de publier un tarif, s'il couvre le coût qu'il autorise.
			if margin, ok := p.UnitMargin(pack.Credits, pack.PriceEUR); ok {
				row.Margin = fmt.Sprintf("%.0f %%", margin)
				switch {
				case margin < 0:
					row.MarginTone = "crit"
					page.LossMaking++
				case margin < p.TargetMargin:
					row.MarginTone = "warn"
				}
				if margin < p.TargetMargin {
					row.Recommended = core.FormatEuros(p.RecommendedPrice(pack.Credits))
				}
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

	h.Render(w, r, http.StatusOK, view.AdminPricing(page))
}

// trimFloat rend un nombre sans zéros inutiles, pour un champ de saisie.
func trimFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// HandlePricingPackCreate ajoute une offre.
func (h *Handlers) HandlePricingPackCreate(w http.ResponseWriter, r *http.Request) {
	credits, errCredits := strconv.ParseInt(r.PostFormValue("credits"), 10, 64)
	if errCredits != nil || credits <= 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	// Le prix est facultatif : laissé vide, il découle du coût réel d'un
	// crédit et de la marge visée. C'est le cas courant — fixer un prix à la
	// main n'a de sens que pour une offre d'appel ou un tarif rond imposé
	// par ailleurs.
	rawPrice := strings.TrimSpace(r.PostFormValue("price_eur"))

	var price float64
	if rawPrice != "" {
		parsed, err := strconv.ParseFloat(strings.Replace(rawPrice, ",", ".", 1), 64)
		if err != nil || parsed < 0 {
			http.Redirect(w, r, "/admin/pricing", http.StatusFound)
			return
		}
		price = parsed
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		if rawPrice == "" {
			p, err := h.Pricing(r.Context(), tx)
			if err != nil {
				return err
			}
			price = p.RecommendedPrice(credits)
		}

		packs, err := h.PricingRepo.ListPacks(r.Context(), tx)
		if err != nil {
			return err
		}

		return h.PricingRepo.InsertPack(r.Context(), tx, persistence.CreditPack{
			Credits:   credits,
			PriceEUR:  price,
			Position:  len(packs),
			CreatedAt: h.Now(),
		})
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: offre de crédits ajoutée",
		"credits", credits, "price_computed", rawPrice == "")
	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// HandlePricingPackDelete retire une offre.
func (h *Handlers) HandlePricingPackDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.PricingRepo.DeletePack(r.Context(), tx, id)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// HandlePricingPackFeature met une offre en avant, une seule à la fois.
func (h *Handlers) HandlePricingPackFeature(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		if err := h.PricingRepo.ClearFeatured(r.Context(), tx); err != nil {
			return err
		}
		return h.PricingRepo.SetFeatured(r.Context(), tx, id)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// HandlePricingSettings enregistre les réglages de conversion.
func (h *Handlers) HandlePricingSettings(w http.ResponseWriter, r *http.Request) {
	now := h.Now()

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		for key, raw := range map[string]string{
			persistence.SettingUSDPerCredit:       r.PostFormValue("usd_per_credit"),
			persistence.SettingEURPerUSD:          r.PostFormValue("eur_per_usd"),
			persistence.SettingWelcomeCredits:     r.PostFormValue("welcome_credits"),
			persistence.SettingDefaultAllowance:   r.PostFormValue("default_allowance"),
			persistence.SettingDefaultInputPrice:  r.PostFormValue("default_input"),
			persistence.SettingDefaultOutputPrice: r.PostFormValue("default_output"),
			persistence.SettingTargetMargin:       r.PostFormValue("target_margin"),
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
			if err := h.PricingRepo.SetSetting(r.Context(), tx, key, value, now); err != nil {
				return err
			}
		}

		return nil
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: réglages de tarification enregistrés")
	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// HandleModelPriceUpsert enregistre le tarif de repli d'un modèle.
func (h *Handlers) HandleModelPriceUpsert(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.PostFormValue("model"))
	input, errInput := parseRate(r.PostFormValue("input"))
	output, errOutput := parseRate(r.PostFormValue("output"))

	if model == "" || errInput != nil || errOutput != nil || input < 0 || output < 0 {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.ModelPrices.Upsert(r.Context(), tx, persistence.ModelPrice{
			Model:            model,
			InputPerMillion:  input,
			OutputPerMillion: output,
			UpdatedAt:        h.Now(),
		})
	})
	if !ok {
		return
	}

	h.Logger.InfoContext(r.Context(), "web: tarif de modèle enregistré", "model", model)
	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// HandleModelPriceDelete retire le tarif d'un modèle.
func (h *Handlers) HandleModelPriceDelete(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.PostFormValue("model"))
	if model == "" {
		http.Redirect(w, r, "/admin/pricing", http.StatusFound)
		return
	}

	ok := h.WithTx(w, r, func(tx *sql.Tx) error {
		return h.ModelPrices.Delete(r.Context(), tx, model)
	})
	if !ok {
		return
	}

	http.Redirect(w, r, "/admin/pricing?saved=1", http.StatusFound)
}

// parseRate lit un tarif saisi, virgule décimale acceptée.
func parseRate(raw string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(strings.Replace(raw, ",", ".", 1)), 64)
}
