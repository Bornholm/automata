package config

import "testing"

// La forme abrégée `organization:` et la liste `organizations:` doivent se
// lire de la même façon : tout le code applicatif passe par
// AllOrganizations, et une configuration construite en mémoire (tests,
// outillage) doit se comporter comme une configuration chargée.
func TestAllOrganizations_MergesBothDeclarationForms(t *testing.T) {
	cfg := &Config{Organization: Organization{ID: "home", DisplayName: "Maison"}}

	orgs := cfg.AllOrganizations()
	if len(orgs) != 1 || orgs[0].ID != "home" {
		t.Fatalf("forme abrégée: AllOrganizations() = %+v, attendu la seule organisation home", orgs)
	}

	cfg.Organizations = []Organization{
		{ID: "home", DisplayName: "Maison"},
		{ID: "work", DisplayName: "Bureau"},
	}

	orgs = cfg.AllOrganizations()
	if len(orgs) != 2 {
		t.Fatalf("AllOrganizations() = %+v, attendu 2 organisations sans doublon", orgs)
	}

	if name := cfg.OrganizationDisplayName("work"); name != "Bureau" {
		t.Errorf("OrganizationDisplayName(work) = %q, attendu %q", name, "Bureau")
	}

	// Un nom absent ne doit pas produire une ligne vide dans le bloc de
	// contexte envoyé au modèle.
	if name := cfg.OrganizationDisplayName("inconnue"); name != "inconnue" {
		t.Errorf("OrganizationDisplayName(inconnue) = %q, attendu le repli sur l'identifiant", name)
	}
}

// Un principal sans rattachement explicite appartient à l'organisation
// unique de l'instance ; dès qu'il y en a plusieurs, l'appartenance doit
// être déclarée, faute de quoi un collègue atteindrait la mémoire de la
// famille.
func TestPrincipalInOrganization(t *testing.T) {
	cfg := &Config{
		Organization: Organization{ID: "home", DisplayName: "Maison"},
		Identities: Identities{
			Principals: []Principal{{ID: "wpetit", Kind: PrincipalKindHuman}},
		},
	}

	if !cfg.PrincipalInOrganization("wpetit", "home") {
		t.Error("mono-organisation: le principal doit appartenir à l'organisation unique")
	}

	cfg.Organizations = []Organization{{ID: "home"}, {ID: "work"}}
	cfg.Identities.Principals = []Principal{
		{ID: "wpetit", Kind: PrincipalKindHuman, Orgs: []string{"home", "work"}},
		{ID: "yann", Kind: PrincipalKindHuman, Orgs: []string{"work"}},
	}

	if !cfg.PrincipalInOrganization("yann", "work") {
		t.Error("yann doit appartenir à work")
	}
	if cfg.PrincipalInOrganization("yann", "home") {
		t.Error("yann ne doit PAS appartenir à home")
	}
	if cfg.PrincipalInOrganization("inconnu", "home") {
		t.Error("un principal inconnu n'appartient à aucune organisation")
	}
}

func TestValidateOrganizations_RequiresAtLeastOneAndUniqueIDs(t *testing.T) {
	assertHasError(t, validateOrganizations(&Config{}), "au moins une organisation est requise")

	cfg := &Config{Organizations: []Organization{{ID: "home"}, {ID: "home"}, {ID: ""}}}
	errs := validateOrganizations(cfg)
	assertHasError(t, errs, "identifiant dupliqué")
	assertHasError(t, errs, "id: requis")
}

// Un org_id mal orthographié dans un canal ne se manifesterait qu'au premier
// message reçu, sous la forme d'un refus d'autorisation sans rapport visible
// avec sa cause : la validation doit le rattraper au chargement.
func TestValidateChannels_ChecksOrganizationMembership(t *testing.T) {
	cfg := &Config{
		Organizations: []Organization{{ID: "home"}, {ID: "work"}},
		Identities: Identities{
			Principals: []Principal{
				{ID: "wpetit", Kind: PrincipalKindHuman, Orgs: []string{"home", "work"}},
				{ID: "yann", Kind: PrincipalKindHuman, Orgs: []string{"work"}},
			},
		},
		Channels: []Channel{
			{Provider: "whatsapp", ChannelID: "a", Kind: ChannelKindPrivate, OrgID: "maison", Scope: ScopePersonal, PrincipalID: "wpetit"},
			{Provider: "whatsapp", ChannelID: "b", Kind: ChannelKindGroup, OrgID: "home", Scope: ScopeGroup, Activation: "mention", Members: []string{"wpetit", "yann"}},
			{Provider: "whatsapp", ChannelID: "c", Kind: ChannelKindPrivate, Scope: ScopePersonal, PrincipalID: "wpetit"},
		},
	}

	errs := validateChannels(cfg)
	assertHasError(t, errs, `channels[0].org_id: organisation inconnue "maison"`)
	assertHasError(t, errs, `channels[1].members: le principal "yann" n'appartient pas à l'organisation "home"`)
	assertHasError(t, errs, "channels[2].org_id: requis")
}

func TestValidateIdentities_RequiresExplicitOrgsWhenSeveralDeclared(t *testing.T) {
	cfg := &Config{
		Organizations: []Organization{{ID: "home"}, {ID: "work"}},
		Identities: Identities{
			Principals: []Principal{
				{ID: "wpetit", Kind: PrincipalKindHuman},
				{ID: "yann", Kind: PrincipalKindHuman, Orgs: []string{"bureau"}},
			},
		},
	}

	errs := validateIdentities(cfg)
	assertHasError(t, errs, "identities.principals[0].orgs: requis dès que plusieurs organisations sont déclarées")
	assertHasError(t, errs, `identities.principals[1].orgs: organisation inconnue "bureau"`)
}
