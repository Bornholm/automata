package conversation

import "testing"

// L'annotation ne dépend que du fait structurel — ce tour avait des outils
// et n'en a appelé aucun — jamais de ce que le message dit. Deux textes
// opposés, même traitement : c'est ce qui la rend incapable de se tromper
// sur le sens.
func TestAnnotateToollessIgnoresWhatTheMessageSays(t *testing.T) {
	texts := []string{
		"Le service de profil n'est pas disponible en ce moment.",
		"Le service de profil n'a pas accepté la requête pour l'instant.",
		"Bonjour ! Comment puis-je t'aider ?",
		"Il fera 21 degrés demain matin.",
	}

	for _, text := range texts {
		if got := annotateToolless(text, true); got != text+toollessMarker {
			t.Errorf("sans appel d'outil, attendu annoté\n  texte  %q\n  obtenu %q", text, got)
		}
		// Un tour qui a appelé un outil a observé quelque chose : son
		// « je ne peux pas » est un constat, et l'annoter dirait au modèle
		// de réessayer ce qui vient d'échouer.
		if got := annotateToolless(text, false); got != text {
			t.Errorf("après un appel d'outil, attendu intact\n  texte  %q\n  obtenu %q", text, got)
		}
	}
}

func TestAnnotateToollessLeavesEmptyTextAlone(t *testing.T) {
	if got := annotateToolless("", true); got != "" {
		t.Fatalf("texte vide annoté: %q", got)
	}
}

// L'annotation ne s'adresse qu'au modèle. S'il la recopie dans sa réponse —
// ce qui est arrivé au marqueur de caviardage — la personne ne doit pas la
// lire.
func TestStripToollessMarker(t *testing.T) {
	annotated := annotateToolless("Le service est indisponible.", true)

	if got := stripToollessMarker(annotated); got != "Le service est indisponible." {
		t.Fatalf("marqueur mal retiré: %q", got)
	}

	const clean = "Voici votre lien."
	if got := stripToollessMarker(clean); got != clean {
		t.Fatalf("texte sans marqueur modifié: %q", got)
	}
}
