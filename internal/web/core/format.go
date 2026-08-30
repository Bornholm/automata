package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/web/view"
)

// Helpers de présentation partagés par plusieurs surfaces : l'écran d'une
// organisation et la page de profil d'un membre montrent les mêmes canaux
// et les mêmes montants.

// formatEuros rend un prix : « 35 € », « 8,50 € ».
func FormatEuros(amount float64) string {
	if amount == float64(int64(amount)) {
		return fmt.Sprintf("%d €", int64(amount))
	}
	return strings.Replace(fmt.Sprintf("%.2f €", amount), ".", ",", 1)
}

// memberChannels retourne les canaux où le membre est présent : ceux de
// la configuration (après bootstrap, les identifiants coïncident avec les
// principals) et ceux rattachés en ligne — sa conversation privée, et
// les groupes de son organisation. Sans les seconds, la fiche d'un membre
// rattaché par jeton n'affichait aucun canal.
func (s *Deps) MemberChannels(ctx context.Context, q persistence.Querier, memberID, orgID string) []view.OrgChannelRow {
	var rows []view.OrgChannelRow

	if orgID != "" {
		bound, err := s.Bindings.ListByOrg(ctx, q, orgID)
		if err != nil {
			// L'absence de cette liste ne justifie pas de refuser la
			// fiche entière : le reste de la page reste juste.
			s.Logger.ErrorContext(ctx, "web: lecture des canaux d'un membre", "member_id", memberID, "error", err)
		}
		for _, binding := range bound {
			// Un canal privé n'appartient qu'à son membre ; un groupe est
			// celui de toute l'organisation.
			if binding.MemberID != "" && binding.MemberID != memberID {
				continue
			}
			rows = append(rows, view.OrgChannelRow{
				PlatformType: s.ProviderTypeOf(binding.Provider),
				Name:         binding.DisplayName,
				Kind:         ChannelKindLabelFromScope(binding.Kind),
			})
		}
	}

	for _, ch := range s.Cfg.Channels {
		if ch.PrincipalID != memberID && !slices.Contains(ch.Members, memberID) {
			continue
		}
		rows = append(rows, view.OrgChannelRow{
			PlatformType: s.ProviderTypeOf(ch.Provider),
			Name:         s.ChannelDisplayName(ch),
			Kind:         ChannelKindLabel(ch.Kind),
		})
	}
	return rows
}

// channelKindLabelFromScope traduit le genre d'un canal lié dynamiquement.
func ChannelKindLabelFromScope(kind string) string {
	if kind == "group" {
		return "Groupe"
	}
	return "Privé"
}
