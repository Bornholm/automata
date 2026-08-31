package main

import (
	"strings"
	"testing"
)

// Le sous-agent doit savoir qu'il a le droit de ne rien dire : sans cette
// permission, il rend compte de tout et résume consciencieusement les
// pourriels, jusqu'à ce que la personne ignore aussi ce qui comptait.
func TestTriggerInput_GrantsPermissionToStaySilent(t *testing.T) {
	input := triggerInput(emailSummary{From: "promo@boutique.test", Subject: "-70 % ce week-end", UID: 42}, "")

	if !strings.Contains(input, "report nothing at all") {
		t.Error("la consigne doit autoriser explicitement le silence")
	}
	if !strings.Contains(input, "email_read with id 42") {
		t.Error("la consigne doit nommer l'identifiant à lire")
	}
}

// Les consignes du membre arrivent APRÈS les règles générales et l'emportent
// sur elles : personne d'autre ne sait que telle infolettre compte.
func TestTriggerInput_MemberInstructionsComeLastAndWin(t *testing.T) {
	instructions := "Ignore les infolettres, sauf celle du syndicat."
	input := triggerInput(emailSummary{From: "info@syndicat.test", Subject: "Réunion", UID: 7}, instructions)

	if !strings.Contains(input, instructions) {
		t.Fatal("les consignes du membre doivent figurer dans la demande")
	}
	if !strings.Contains(input, "take precedence") {
		t.Error("la consigne doit dire que les instructions du membre priment")
	}
	if strings.Index(input, instructions) < strings.Index(input, "report nothing at all") {
		t.Error("les consignes du membre doivent venir après les règles générales")
	}
}

// Sans consigne, rien n'est ajouté : une section vide inviterait le modèle à
// inventer des règles qu'on ne lui a pas données.
func TestTriggerInput_WithoutInstructionsAddsNothing(t *testing.T) {
	input := triggerInput(emailSummary{From: "a@b.test", Subject: "Bonjour", UID: 1}, "   ")

	if strings.Contains(input, "standing orders") {
		t.Error("aucune section de consignes ne doit apparaître quand il n'y en a pas")
	}
}
