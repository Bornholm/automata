package main

import "testing"

// Le mot-clé de traitement a un défaut : sans lui, aucun marquage n'aurait
// lieu et rien ne distinguerait un courriel déjà vu par l'agent.
func TestProcessedLabel_HasADefault(t *testing.T) {
	if got := (memberConfig{}).processedLabel(); got != defaultProcessedLabel {
		t.Errorf("mot-clé = %q, attendu %q", got, defaultProcessedLabel)
	}
	if got := (memberConfig{ProcessedLabel: "Vu"}).processedLabel(); got != "Vu" {
		t.Errorf("mot-clé = %q, attendu celui du membre", got)
	}
}

// Consignes et mot-clé survivent à l'aller-retour JSON : ils vivent dans la
// configuration scellée du membre, relue à chaque tour.
func TestMemberConfig_KeepsInstructionsAndLabel(t *testing.T) {
	original := memberConfig{
		IMAPHost: "imap.test", Username: "cam", AllowRead: true,
		Instructions:   "Ignore les infolettres.",
		ProcessedLabel: "Traité",
	}

	restored, err := parseConfig(original.marshal())
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if restored.Instructions != original.Instructions {
		t.Errorf("consignes = %q, attendu %q", restored.Instructions, original.Instructions)
	}
	if restored.ProcessedLabel != original.ProcessedLabel {
		t.Errorf("mot-clé = %q, attendu %q", restored.ProcessedLabel, original.ProcessedLabel)
	}
}
