package persistence

import "time"

// Ce fichier définit les DTO des tables du socle SaaS (migration 0010) :
// organisations, membres, jetons de liaison, portefeuille de crédits et
// liens de profil. Contrairement aux DTO historiques de types.go (chaînes
// RFC3339 brutes), les horodatages sont des time.Time : la couche web
// compare des échéances (expiration de jeton, de lien) et convertir partout
// chez les appelants multiplierait les occasions de se tromper. Les valeurs
// zéro signifient « non renseigné » (ex. LinkedAt zéro = membre pas encore
// lié à une identité de messagerie).

// Organization est le DTO de la table organizations.
type Organization struct {
	ID          string
	DisplayName string
	// Offered marque une organisation « offerte par la maison » :
	// allocation mensuelle de crédits non cumulative au lieu d'achats.
	Offered          bool
	MonthlyAllowance int64
	// Unlimited marque une organisation gratuite SANS limite : jamais
	// débitée, jamais mise en pause, jamais alertée sur son solde. Sa
	// consommation reste mesurée (usage_records), donc son coût réel
	// demeure visible dans les écrans d'usage et de marge. Distinct
	// d'Offered, qui plafonne à une allocation mensuelle.
	Unlimited bool
	// LowBalanceNotifiedAt date la dernière alerte de solde bas envoyée
	// dans la conversation ; zéro = jamais prévenue.
	LowBalanceNotifiedAt time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Rôles d'un membre au sein de son organisation (members.role).
const (
	MemberRoleMember   = "member"
	MemberRoleOwner    = "owner"
	MemberRoleReadOnly = "readonly"
)

// Member est le DTO de la table members.
type Member struct {
	ID          string
	OrgID       string
	DisplayName string
	Role        string
	Email       string
	// EmailVerifiedAt zéro = adresse non vérifiée.
	EmailVerifiedAt time.Time
	// Provider/ExternalUserID/LinkedAt : identité de messagerie rattachée
	// après consommation du jeton (lot B) ou par le bootstrap.
	Provider       string
	ExternalUserID string
	LinkedAt       time.Time
	// OnboardingState suit la visite d'accueil (migration 0025) : ''
	// jamais proposée, 'offered', 'q1'…, 'done', 'skipped'. Il vit sur le
	// membre, pas sur la conversation : changer de canal ne refait pas la
	// visite, et l'écarter vaut partout.
	OnboardingState string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Linked indique si le membre est rattaché à une identité de messagerie.
func (m Member) Linked() bool { return !m.LinkedAt.IsZero() }

// Natures et états d'un jeton de liaison (link_tokens.kind / .status).
// L'état « expiré » se calcule depuis ExpiresAt, il n'est jamais stocké.
const (
	LinkTokenKindPersonal = "personal"
	LinkTokenKindGroup    = "group"

	LinkTokenStatusPending = "pending"
	LinkTokenStatusUsed    = "used"
	LinkTokenStatusRevoked = "revoked"
)

// LinkToken est le DTO de la table link_tokens. TokenHash est le SHA-256
// hexadécimal du jeton : le clair n'existe qu'au moment de la création.
type LinkToken struct {
	ID        string
	Kind      string
	MemberID  string
	OrgID     string
	TokenHash string
	Status    string
	ExpiresAt time.Time
	UsedAt    time.Time
	CreatedAt time.Time
}

// Expired indique si le jeton est périmé à l'instant now.
func (t LinkToken) Expired(now time.Time) bool {
	return t.Status == LinkTokenStatusPending && now.After(t.ExpiresAt)
}

// Natures d'un mouvement du portefeuille (wallet_entries.kind).
const (
	WalletKindPurchase   = "purchase"
	WalletKindGrant      = "grant"
	WalletKindWelcome    = "welcome"
	WalletKindAllowance  = "allowance"
	WalletKindUsage      = "usage"
	WalletKindAdjustment = "adjustment"
)

// WalletEntry est le DTO de la table wallet_entries : une ligne immuable
// du livre de comptes, montant signé en crédits.
type WalletEntry struct {
	ID        int64
	OrgID     string
	Kind      string
	Label     string
	Amount    int64
	CreatedAt time.Time
	// ExternalRef identifie l'origine externe du mouvement (session de
	// paiement Stripe) : unique en base, elle garantit qu'un paiement
	// rejoué n'est jamais crédité deux fois. Vide pour les mouvements
	// internes (usage, allocation, geste commercial).
	ExternalRef string
	// PriceEUR est la somme réellement payée pour cet apport, en euros.
	// Sans elle, le portefeuille dirait combien de crédits ont été vendus
	// mais pas ce qu'ils ont rapporté. Zéro hors achat.
	PriceEUR float64
}

// États d'un lien de profil (profile_links.status) : usage unique à
// l'ouverture, « expiré » calculé depuis ExpiresAt.
const (
	ProfileLinkStatusPending = "pending"
	ProfileLinkStatusOpened  = "opened"
)

// ProfileLink est le DTO de la table profile_links.
type ProfileLink struct {
	ID        string
	MemberID  string
	TokenHash string
	Status    string
	ExpiresAt time.Time
	OpenedAt  time.Time
	CreatedAt time.Time
}
