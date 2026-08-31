package pluginsdk

import "testing"

func TestIsTriggerSilent(t *testing.T) {
	silent := []string{
		"NOTHING_TO_REPORT",
		"nothing_to_report",
		"  NOTHING_TO_REPORT  ",
		"NOTHING_TO_REPORT.",
		"**NOTHING_TO_REPORT**",
		"\"NOTHING_TO_REPORT\"",
	}
	for _, reply := range silent {
		if !IsTriggerSilent(reply) {
			t.Errorf("IsTriggerSilent(%q) = false, attendu true", reply)
		}
	}

	// Un marqueur pris dans une phrase reste une réponse : la personne a
	// quelque chose à lire, et l'escamoter serait pire que de trop dire.
	loud := []string{
		"",
		"Un courriel de Lina au sujet du contrat.",
		"Ce message n'est pas un NOTHING_TO_REPORT, il demande une réponse.",
		"NOTHING_TO_REPORT for the first email, but the second needs an answer.",
	}
	for _, reply := range loud {
		if IsTriggerSilent(reply) {
			t.Errorf("IsTriggerSilent(%q) = true, attendu false", reply)
		}
	}
}
