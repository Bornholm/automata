package admin

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestUsageQuery_DefaultsToCurrentMonthByOrg(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/admin/usage", nil)

	from, to, dimensions := usageQuery(r, now)

	if from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("période par défaut inattendue: %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	if len(dimensions) != 1 || dimensions[0] != "org" {
		t.Errorf("dimensions par défaut inattendues: %v", dimensions)
	}
}

func TestUsageQuery_ReadsFiltersAndRejectsUnknownDimensions(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/admin/usage?from=2026-07-01&to=2026-08-01&by=agent&by=model&by=DROP", nil)

	from, to, dimensions := usageQuery(r, now)

	if from.Format("2006-01-02") != "2026-07-01" || to.Format("2006-01-02") != "2026-08-01" {
		t.Errorf("période inattendue: %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
	// Une dimension hors liste blanche est ignorée : elle serait sinon
	// interpolée dans la requête SQL par le repository.
	if len(dimensions) != 2 || dimensions[0] != "agent" || dimensions[1] != "model" {
		t.Errorf("dimensions inattendues: %v", dimensions)
	}
}

// Une date illisible ne doit pas vider l'écran : on retombe sur le mois
// courant plutôt que d'afficher une période absurde.
func TestUsageQuery_IgnoresUnparsableDates(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest("GET", "/admin/usage?from=hier&to=demain", nil)

	from, to, _ := usageQuery(r, now)

	if from.Format("2006-01-02") != "2026-08-01" || to.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("période inattendue: %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))
	}
}

func TestTrimFloat_KeepsInputReadable(t *testing.T) {
	for value, expected := range map[float64]string{
		0.001: "0.001",
		0.92:  "0.92",
		1:     "1",
	} {
		if got := trimFloat(value); got != expected {
			t.Errorf("trimFloat(%v) = %q, attendu %q", value, got, expected)
		}
	}
}
