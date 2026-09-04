package config

import (
	"fmt"
	"slices"
	"time"

	"github.com/dustin/go-humanize"
)

// Duration enveloppe time.Duration pour permettre un unmarshalling YAML à
// partir d'une chaîne ("5s", "2m", "10m").
type Duration time.Duration

// UnmarshalYAML implémente yaml.Unmarshaler pour Duration.
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("durée invalide %q: %w", s, err)
	}

	*d = Duration(parsed)

	return nil
}

// Duration retourne la valeur time.Duration sous-jacente.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// ByteSize enveloppe une taille en octets pour permettre un unmarshalling
// YAML à partir d'une chaîne ("20MiB", "16KiB", "64KiB").
type ByteSize uint64

// UnmarshalYAML implémente yaml.Unmarshaler pour ByteSize.
func (b *ByteSize) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}

	parsed, err := humanize.ParseBytes(s)
	if err != nil {
		return fmt.Errorf("taille invalide %q: %w", s, err)
	}

	*b = ByteSize(parsed)

	return nil
}

// Bytes retourne la valeur uint64 sous-jacente.
func (b ByteSize) Bytes() uint64 {
	return uint64(b)
}

// Config est la racine de la configuration YAML d'Automata.
type Config struct {
	Version int `yaml:"version"`
	// Organizations déclare les organisations servies par cette instance.
	// Chaque canal désigne la sienne par channels[].org_id, et aucune donnée
	// ne traverse cette frontière (voir internal/authorization).
	Organizations []Organization `yaml:"organizations"`
	// Organization est la forme abrégée acceptée quand l'instance ne sert
	// qu'une organisation. Elle est équivalente à une liste d'un élément :
	// toute lecture passe par AllOrganizations, jamais par ce champ.
	Organization Organization `yaml:"organization"`
	Storage      Storage      `yaml:"storage"`
	Courier      Courier      `yaml:"courier"`
	Audio        Audio        `yaml:"audio"`
	Attachments  Attachments  `yaml:"attachments"`
	// Les clients de modèles (fournisseur, modèle, clé) ne se déclarent
	// PLUS ici : le catalogue vit en base et s'administre en ligne
	// (/admin/llm-clients), tout comme l'affectation d'un client à chaque
	// rôle. Voir docs/models.md — le YAML ne décrit que la machine.
	Agents        map[string]Agent     `yaml:"agents"`
	MCPServers    map[string]MCPServer `yaml:"mcp_servers"`
	Memory        Memory               `yaml:"memory"`
	Conversation  Conversation         `yaml:"conversation"`
	Identities    Identities           `yaml:"identities"`
	Origins       []Origin             `yaml:"origins"`
	Channels      []Channel            `yaml:"channels"`
	Schedules     []Schedule           `yaml:"schedules"`
	Observability Observability        `yaml:"observability"`
	Web           Web                  `yaml:"web"`
	Backup        Backup               `yaml:"backup"`
	Plugins       Plugins              `yaml:"plugins"`
	Introspection Introspection        `yaml:"introspection"`
	// DefaultLocale est la langue des textes qu'Automata écrit sans passer
	// par un modèle, pour qui n'en a pas choisi une : « fr », « en » ou
	// « es ». Vide = français.
	//
	// Elle ne dit RIEN de la langue des réponses : celles-ci suivent depuis
	// toujours la langue du message reçu (prompts/main.md). Ce réglage ne
	// couvre que les messages fabriqués par le code — repli d'erreur,
	// propositions d'actions, visite d'accueil, pages web, courriels.
	DefaultLocale string `yaml:"default_locale"`
}

// Introspection décrit la passe hebdomadaire qui relit les frictions
// d'usage (plans d'actions jamais confirmés, rappels en échec, tâches en
// échec) et les motifs comportementaux déjà en mémoire, pour proposer à
// chaque membre au plus une amélioration : automatiser un geste répété,
// activer une capacité inutilisée, corriger ce qui échoue. Désactivée par
// défaut ; le modèle se règle en ligne (rôle « introspection »).
type Introspection struct {
	Enabled bool `yaml:"enabled"`
	// Cron est l'échéance de la passe par membre. Vide applique le défaut
	// ("20 5 * * 1", chaque lundi matin) : l'introspection a besoin d'une
	// semaine de matière, pas d'une nuit.
	Cron string `yaml:"cron"`
	// DigestCron est l'échéance de la synthèse anonyme envoyée à
	// l'exploitant. Vide applique le défaut ("50 5 1 * *", le premier du
	// mois).
	DigestCron string `yaml:"digest_cron"`
}

// Backup décrit les sauvegardes périodiques des bases SQLite. Désactivées
// par défaut ; activées, elles couvrent la base applicative, la mémoire et
// les sessions de messagerie — perdre l'une d'elles coûte respectivement
// les conversations et les portefeuilles, les souvenirs, ou un
// ré-appairage de chaque compte.
type Backup struct {
	Enabled bool `yaml:"enabled"`
	// Directory reçoit les copies ; il doit vivre hors du répertoire de
	// données, idéalement sur un autre support.
	Directory string `yaml:"directory"`
	// Interval sépare deux sauvegardes. Vide : six heures.
	Interval Duration `yaml:"interval"`
	// Keep borne le nombre de copies conservées par base. Zéro : dix.
	Keep int `yaml:"keep"`
	// ExtraPaths ajoute des bases SQLite à sauvegarder (sessions de
	// messagerie, index annexes) : nom d'affichage → chemin.
	ExtraPaths map[string]string `yaml:"extra_paths"`
}

// EffectiveInterval retourne l'intervalle configuré, ou six heures.
func (b Backup) EffectiveInterval() time.Duration {
	if interval := b.Interval.Duration(); interval > 0 {
		return interval
	}
	return 6 * time.Hour
}

// EffectiveKeep retourne la rétention configurée, ou dix copies.
func (b Backup) EffectiveKeep() int {
	if b.Keep > 0 {
		return b.Keep
	}
	return 10
}

// Observability décrit le serveur HTTP local optionnel de santé et de
// métriques (plan de conception, Phase 20). Désactivé par défaut : une section absente
// (ou Enabled: false) ne démarre aucun serveur HTTP. Aucune valeur par
// défaut n'est inventée pour Addr au-delà de cette désactivation par
// défaut ; lorsque Enabled est vrai, Addr est requis.
type Observability struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
}

// Web décrit le serveur web d'administration et de profil (socle SaaS,
// maquettes P1). Désactivé par défaut, comme Observability. Lorsque
// Enabled est vrai : Addr, BaseURL, SessionSecret et les identifiants
// d'opérateur sont requis (voir validateWeb).
type Web struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"`
	// BaseURL est l'URL publique servant à composer les liens de profil
	// envoyés dans les conversations (ex: https://automata.exemple.fr).
	BaseURL string `yaml:"base_url"`
	// SessionSecret signe les cookies de session (HMAC-SHA256). Au moins
	// 32 octets ; passe par une variable d'environnement, jamais en clair.
	SessionSecret string   `yaml:"session_secret"`
	Admin         WebAdmin `yaml:"admin"`
	// MailProvider est le nom d'un provider courier de type "mail" utilisé
	// pour envoyer les codes de vérification de courriel (PRO-01).
	// Optionnel : sans lui, la vérification est proposée mais désactivée.
	MailProvider string     `yaml:"mail_provider"`
	Credits      WebCredits `yaml:"credits"`
	Stripe       WebStripe  `yaml:"stripe"`
}

// WebStripe configure le paiement des packs de crédits. Vide : les boutons
// d'achat restent affichés mais inertes (« prochainement »), le portefeuille
// ne se recharge alors que par geste commercial ou allocation.
type WebStripe struct {
	// SecretKey est la clé secrète du compte (sk_…), jamais exposée aux
	// pages : elle ne sert qu'aux appels serveur vers l'API Stripe.
	SecretKey string `yaml:"secret_key"`
	// WebhookSecret (whsec_…) vérifie la signature des événements reçus :
	// sans elle, n'importe qui pourrait créditer un portefeuille.
	WebhookSecret string `yaml:"webhook_secret"`
	// TaxCode est le code fiscal Stripe des crédits vendus. Il n'est exigé
	// que si Stripe Tax est activé sur le compte, mais le fournir dans
	// tous les cas ne coûte rien. Défaut : services fournis par voie
	// électronique (voir EffectiveTaxCode).
	TaxCode string `yaml:"tax_code"`
}

// defaultStripeTaxCode classe les crédits comme un service fourni par voie
// électronique — ce que vend Automata. Un code plus précis (SaaS
// professionnel, par exemple) se règle par configuration selon le régime
// applicable, qui relève du comptable, pas du logiciel.
const defaultStripeTaxCode = "txcd_10000000"

// EffectiveTaxCode retourne le code fiscal configuré, ou le défaut.
func (s WebStripe) EffectiveTaxCode() string {
	if s.TaxCode != "" {
		return s.TaxCode
	}
	return defaultStripeTaxCode
}

// Enabled indique si le paiement en ligne est configuré.
func (s WebStripe) Enabled() bool {
	return s.SecretKey != "" && s.WebhookSecret != ""
}

// WebAdmin décrit le compte opérateur de l'interface d'administration.
type WebAdmin struct {
	Email string `yaml:"email"`
	// PasswordHash est un hachage bcrypt, produit par la sous-commande
	// "automata web hash-password". Jamais de mot de passe en clair.
	PasswordHash string `yaml:"password_hash"`
}

// WebCredits décrit l'économie provisoire de la monnaie virtuelle côté
// affichage (l'écran de pilotage ADM-08 arrive dans un lot ultérieur).
type WebCredits struct {
	// USDPerCredit convertit le coût réel mesuré (usage_records, USD) en
	// crédits pour l'affichage de la consommation. Défaut : 0.001 (1 000
	// crédits par dollar) via EffectiveUSDPerCredit.
	USDPerCredit float64 `yaml:"usd_per_credit"`
	// Packs sont les offres d'achat affichées sur le profil (PRO-02). Le
	// paiement Stripe arrive dans un lot ultérieur.
	Packs []WebCreditPack `yaml:"packs"`
}

// EffectiveUSDPerCredit retourne le taux configuré, ou le défaut 0.001.
func (c WebCredits) EffectiveUSDPerCredit() float64 {
	if c.USDPerCredit > 0 {
		return c.USDPerCredit
	}
	return 0.001
}

// WebCreditPack est une offre d'achat de crédits.
type WebCreditPack struct {
	Credits  int64   `yaml:"credits"`
	PriceEUR float64 `yaml:"price_eur"`
	// Featured met le pack en avant (« Le plus choisi » sur PRO-02).
	Featured bool `yaml:"featured"`
}

// Organization décrit une organisation servie par l'instance.
type Organization struct {
	ID          string `yaml:"id"`
	DisplayName string `yaml:"display_name"`
}

// AllOrganizations retourne les organisations déclarées, la forme abrégée
// `organization:` comprise et placée en tête. Le résultat est vide si
// aucune organisation n'est déclarée — un cas que la validation refuse.
//
// C'est le seul point de lecture des organisations : les deux formes de
// déclaration sont réconciliées ici, et non par une normalisation au
// chargement, pour qu'une configuration construite en mémoire (tests,
// outillage) se comporte exactement comme une configuration chargée.
func (c *Config) AllOrganizations() []Organization {
	orgs := make([]Organization, 0, len(c.Organizations)+1)

	if c.Organization.ID != "" {
		orgs = append(orgs, c.Organization)
	}

	for _, org := range c.Organizations {
		// La déduplication ne vaut que pour un identifiant réellement
		// déclaré : comparer à une forme abrégée vide ferait disparaître de
		// la liste une organisation sans id, que la validation doit au
		// contraire signaler.
		if c.Organization.ID != "" && org.ID == c.Organization.ID {
			continue
		}

		orgs = append(orgs, org)
	}

	return orgs
}

// LookupOrganization retourne l'organisation déclarée sous cet identifiant.
func (c *Config) LookupOrganization(id string) (Organization, bool) {
	for _, org := range c.AllOrganizations() {
		if org.ID == id {
			return org, true
		}
	}

	return Organization{}, false
}

// OrganizationDisplayName retourne le nom affiché de l'organisation, ou son
// identifiant à défaut : ce nom part vers le modèle dans le bloc de
// contexte, mieux vaut un identifiant technique qu'un champ vide.
func (c *Config) OrganizationDisplayName(id string) string {
	org, ok := c.LookupOrganization(id)
	if !ok || org.DisplayName == "" {
		return id
	}

	return org.DisplayName
}

// PrincipalOrganizations retourne les organisations auxquelles appartient ce
// principal. Un principal sans `orgs` explicite appartient à l'organisation
// unique de l'instance ; dès qu'il y en a plusieurs, la liste est exigée par
// la validation, de sorte qu'un oubli ne donne jamais accès aux deux.
func (c *Config) PrincipalOrganizations(p Principal) []string {
	if len(p.Orgs) > 0 {
		return p.Orgs
	}

	if orgs := c.AllOrganizations(); len(orgs) == 1 {
		return []string{orgs[0].ID}
	}

	return nil
}

// PrincipalInOrganization indique si le principal identifié par principalID
// appartient à l'organisation orgID. Un principal inconnu n'appartient à
// aucune organisation.
func (c *Config) PrincipalInOrganization(principalID, orgID string) bool {
	for _, p := range c.Identities.Principals {
		if p.ID != principalID {
			continue
		}

		return slices.Contains(c.PrincipalOrganizations(p), orgID)
	}

	return false
}

// Storage décrit le stockage applicatif.
// Plugins configure le système de plugins : des binaires découverts dans
// un répertoire, lancés en sous-processus (hashicorp/go-plugin) et
// dialogués en gRPC. Un plugin peut fournir un sous-agent délégué, des
// déclencheurs extérieurs et sa propre interface, activés PAR ORGANISATION
// depuis l'administration.
type Plugins struct {
	Enabled bool `yaml:"enabled"`
	// Dir est le répertoire des binaires de plugins, résolu relativement
	// au fichier de configuration. Tout fichier exécutable y est chargé :
	// le répertoire doit rester sous le contrôle exclusif de l'exploitant.
	Dir string `yaml:"dir"`
	// Le modèle des sous-agents et celui de view_file se règlent en
	// ligne : rôles « plugins » et « plugins.vision » de l'écran des
	// modèles.
	// RestartCooldown est le délai minimal entre deux redémarrages d'un
	// même plugin après incident. Vide : 30s. Il évite qu'un plugin qui
	// meurt en boucle (OOM) consomme la machine.
	RestartCooldown Duration `yaml:"restart_cooldown"`
	// MemLimit est transmis en GOMEMLIMIT aux sous-processus. Vide :
	// aucune limite.
	MemLimit string `yaml:"mem_limit"`
	// Triggers borne les déclenchements extérieurs.
	Triggers PluginTriggers `yaml:"triggers"`
	// ObjectStoreMaxObjectBytes borne la taille d'un objet du magasin des
	// plugins. Vide : 16 Mio.
	ObjectStoreMaxObjectBytes ByteSize `yaml:"object_store_max_object_bytes"`
	// ObjectStoreMaxMemberBytes borne le volume total du magasin pour un
	// (plugin, organisation, membre). Vide : 64 Mio.
	ObjectStoreMaxMemberBytes ByteSize `yaml:"object_store_max_member_bytes"`
}

// PluginTriggers borne les déclenchements : un plugin extérieur peut
// recevoir des rafales (une boîte mail inondée), et chaque déclenchement
// coûte un tour de modèle.
type PluginTriggers struct {
	// MaxPerMinute est le plafond de déclenchements par (plugin,
	// organisation). Vide : 6. Au-delà, les événements sont abandonnés et
	// comptés, jamais mis en file sans borne.
	MaxPerMinute int `yaml:"max_per_minute"`
	// MaxConcurrent est le nombre d'exécutions de sous-agents simultanées
	// tous plugins confondus. Vide : 2.
	MaxConcurrent int `yaml:"max_concurrent"`
}

// EffectiveRestartCooldown retourne le délai de redémarrage, défaut
// compris.
func (p Plugins) EffectiveRestartCooldown() time.Duration {
	if d := p.RestartCooldown.Duration(); d > 0 {
		return d
	}
	return 30 * time.Second
}

// EffectiveMaxPerMinute retourne le plafond par minute, défaut compris.
func (t PluginTriggers) EffectiveMaxPerMinute() int {
	if t.MaxPerMinute > 0 {
		return t.MaxPerMinute
	}
	return 6
}

// EffectiveMaxConcurrent retourne le plafond de concurrence, défaut
// compris.
func (t PluginTriggers) EffectiveMaxConcurrent() int {
	if t.MaxConcurrent > 0 {
		return t.MaxConcurrent
	}
	return 2
}

type Storage struct {
	Application StorageApplication `yaml:"application"`
	// EncryptionKey active le chiffrement au repos des contenus
	// personnels : messages, résumés de conversation, rappels et pièces
	// jointes. Vide, ils sont écrits en clair.
	//
	// Le chiffrement est applicatif, pas moteur : les valeurs partent
	// chiffrées vers la base, donc le réglage suit tel quel vers un autre
	// moteur. Il protège une base volée, une sauvegarde égarée, un disque
	// revendu — pas un processus compromis, qui détient la clé.
	//
	// Perdre cette clé rend les contenus déjà chiffrés définitivement
	// illisibles : elle se sauvegarde à part, et jamais dans le même
	// coffre que les données.
	EncryptionKey string `yaml:"encryption_key"`
}

// StorageApplication décrit la base SQLite applicative.
type StorageApplication struct {
	Driver  string  `yaml:"driver"`
	Path    string  `yaml:"path"`
	Pragmas Pragmas `yaml:"pragmas"`
}

// Pragmas décrit les pragmas SQLite appliqués à l'ouverture.
type Pragmas struct {
	ForeignKeys bool     `yaml:"foreign_keys"`
	JournalMode string   `yaml:"journal_mode"`
	BusyTimeout Duration `yaml:"busy_timeout"`
}

// defaultCoalesceWindow est la fenêtre de coalescence des rafales appliquée
// quand courier.coalesce_window est absent de la configuration. Deux
// secondes absorbent la quasi-totalité des « pensées en plusieurs bulles »
// sans retarder sensiblement la réponse.
const defaultCoalesceWindow = 2 * time.Second

// Courier décrit la configuration des fournisseurs de messagerie. Les
// COMPTES eux-mêmes ne se déclarent plus ici : ils vivent en base (table
// platforms) et se gèrent dans l'administration — le semis silencieux qui
// les réimportait du YAML à chaque démarrage ressuscitait les comptes
// supprimés en ligne.
type Courier struct {
	// CoalesceWindow est la fenêtre de silence attendue avant de traiter un
	// message entrant : les messages texte consécutifs d'un même expéditeur
	// arrivés pendant cette fenêtre sont fusionnés en un seul tour de
	// conversation (voir internal/ingress). Pointeur pour distinguer trois
	// cas : absent (défaut de defaultCoalesceWindow), « 0s » explicite
	// (coalescence désactivée), ou une durée choisie.
	CoalesceWindow *Duration `yaml:"coalesce_window"`
}

// EffectiveCoalesceWindow retourne la fenêtre de coalescence à appliquer,
// défaut compris.
func (c Courier) EffectiveCoalesceWindow() time.Duration {
	if c.CoalesceWindow == nil {
		return defaultCoalesceWindow
	}

	return c.CoalesceWindow.Duration()
}

// CourierProvider décrit un fournisseur Go Courier. Le champ Type est requis,
// les autres champs sont libres et conservés dans Extra.
type CourierProvider struct {
	Type  string                 `yaml:"type"`
	Extra map[string]interface{} `yaml:",inline"`
}

// UnmarshalYAML implémente un décodage manuel pour capturer à la fois le
// champ Type et les champs libres restants dans Extra.
func (p *CourierProvider) UnmarshalYAML(unmarshal func(interface{}) error) error {
	raw := map[string]interface{}{}
	if err := unmarshal(&raw); err != nil {
		return err
	}

	typ, _ := raw["type"].(string)
	p.Type = typ
	delete(raw, "type")
	p.Extra = raw

	return nil
}

// Conversation décrit la gestion de l'historique conversationnel rejoué au
// modèle à chaque tour.
type Conversation struct {
	// HistoryLimit borne le nombre de messages passés rejoués comme
	// contexte. 0 applique le défaut (20, voir internal/conversation).
	HistoryLimit int `yaml:"history_limit"`
	// Compaction condense les messages plus anciens que HistoryLimit en un
	// résumé roulant persisté, réinjecté en tête de contexte.
	Compaction Compaction `yaml:"compaction"`
}

// Compaction décrit la compaction de l'historique conversationnel.
// Désactivée par défaut : sans section explicite, les messages au-delà de la
// fenêtre d'historique sortent simplement du contexte, comme avant.
type Compaction struct {
	Enabled bool `yaml:"enabled"`
	// Le modèle qui produit les résumés se règle en ligne : rôle
	// « compaction » de l'écran des modèles. Un modèle économique suffit
	// largement.
	// MaxSummaryChars borne la taille du résumé persisté. 0 applique le
	// défaut (2000 caractères).
	MaxSummaryChars int `yaml:"max_summary_chars"`
	// ExtractFacts extrait, à chaque compaction, les faits durables des
	// messages condensés et les stocke dans la mémoire à long terme
	// (Amoxtli), dans la portée de la conversation. Requiert Enabled et un
	// système de mémoire configuré.
	ExtractFacts bool `yaml:"extract_facts"`
	// RecordEpisodes conserve, à chaque compaction, le fragment condensé
	// VERBATIM dans la mémoire épisodique (Amoxtli), dans la portée de la
	// conversation — retrouvable ensuite par l'outil
	// search_conversation_history. Requiert Enabled et un système de
	// mémoire configuré.
	RecordEpisodes bool `yaml:"record_episodes"`
	// MaxFacts borne le nombre de faits mémorisés par compaction. 0
	// applique le défaut (5).
	MaxFacts int `yaml:"max_facts"`
}

// Audio décrit la configuration de traitement des messages vocaux.
type Audio struct {
	Enabled bool `yaml:"enabled"`
	// Le modèle de transcription se règle en ligne : rôle
	// « transcription » de l'écran des modèles.
	MaxSize              ByteSize `yaml:"max_size"`
	Timeout              Duration `yaml:"timeout"`
	PersistAudio         bool     `yaml:"persist_audio"`
	PersistTranscription bool     `yaml:"persist_transcription"`
}

// Attachments décrit le traitement des pièces jointes non vocales (images,
// documents) reçues et renvoyées : les notes vocales relèvent d'Audio.
//
// Désactivé par défaut : sans section explicite, une pièce jointe est écartée
// et signalée à l'agent, jamais transmise au modèle à son insu.
type Attachments struct {
	Enabled bool `yaml:"enabled"`
	// MaxSize borne la taille d'UNE pièce jointe reçue.
	MaxSize ByteSize `yaml:"max_size"`
	// MaxCount borne le nombre de pièces jointes retenues par message.
	MaxCount int `yaml:"max_count"`
	// AcceptedTypes énumère les types MIME transmis au modèle. Ce filtre
	// protège le tour entier : un fournisseur refuse la requête complète
	// lorsqu'une pièce jointe ne lui convient pas, laissant l'utilisateur
	// sans réponse. Il doit donc rester aligné sur ce que le modèle visé
	// accepte réellement.
	AcceptedTypes []string `yaml:"accepted_types"`
	// ToolTypes énumère les types MIME conservés POUR LES OUTILS et jamais
	// transmis au modèle : une vidéo reçue par messagerie que le
	// sous-agent d'un plugin ira chercher lui-même. Les déclarer dans
	// accepted_types ferait au contraire échouer le tour entier chez un
	// fournisseur qui ne sait pas lire une vidéo.
	ToolTypes []string `yaml:"tool_types"`
	// MaxToolSize borne la taille d'une pièce jointe tool_types. Distincte
	// de max_size : ces pièces ne coûtent aucun jeton.
	MaxToolSize ByteSize `yaml:"max_tool_size"`
	// MaxHistory borne le nombre de pièces jointes rejouées depuis
	// l'historique à chaque tour, les plus récentes d'abord. Sans cette
	// borne, une conversation riche en images ferait croître indéfiniment la
	// taille (et le coût) de chaque requête.
	MaxHistory int `yaml:"max_history"`
	// MaxReply borne le nombre de pièces jointes renvoyées à l'utilisateur
	// en une réponse, pour qu'un outil prolixe ne l'inonde pas.
	MaxReply int `yaml:"max_reply"`
}

// LLMClient décrit un client LLM configuré.
type LLMClient struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
	BaseURL  string `yaml:"base_url"`
	// Reasoning règle le budget de réflexion des modèles qui en ont un.
	// Absente, la valeur par défaut du modèle s'applique.
	Reasoning *LLMReasoning `yaml:"reasoning"`
	// Vision déclare si le modèle accepte les images en entrée. Absente, la
	// valeur vaut true (comportement historique). À false, les agents
	// utilisant ce client n'envoient JAMAIS de pièce jointe au modèle : un
	// fournisseur texte-seul rejette la requête ENTIÈRE dès qu'un message
	// en contient une (« no endpoints found that support image input »), et
	// la panne emporterait tout le tour. Les pièces jointes continuent
	// d'accompagner les délégations, où un spécialiste multimodal peut les
	// voir.
	Vision *bool `yaml:"vision"`
	// ExtraFields ajoute des paramètres bruts au corps de chaque requête.
	// Les passerelles compatibles OpenAI en acceptent que l'API d'origine
	// ignore — « usage: {include: true} » chez OpenRouter, qui commande le
	// report du coût réel de l'appel. Rien n'est validé ici : le contenu
	// part tel quel au fournisseur.
	ExtraFields map[string]any `yaml:"extra_fields"`
}

// SupportsVision indique si le client accepte les images en entrée (true
// par défaut).
func (c LLMClient) SupportsVision() bool {
	return c.Vision == nil || *c.Vision
}

// LLMReasoning règle la réflexion d'un modèle « reasoning ».
//
// Le réglage vaut pour tous les appels du client : conversation, délégation,
// compaction, consolidation. Réfléchir coûte du temps sur chaque message,
// banalités comprises — un « coucou » peut demander une demi-minute — et un
// assistant de messagerie se juge aussi à sa vivacité.
type LLMReasoning struct {
	// Effort vaut "minimal", "low", "medium", "high" (valeurs documentées
	// par OpenRouter), ou "none"/"xhigh" que certains fournisseurs
	// acceptent. Un modèle sans mode réflexion l'ignore.
	Effort string `yaml:"effort"`
}

// AgentType énumère les types d'agents supportés.
type AgentType string

const (
	AgentTypeOrchestrator AgentType = "orchestrator"
	AgentTypeSpecialist   AgentType = "specialist"
)

// Agent décrit un agent LLM (orchestrateur ou spécialiste).
type Agent struct {
	Type AgentType `yaml:"type"`
	// Description dit, en une phrase, ce que ce spécialiste sait faire.
	// Elle est reprise dans la description de l'outil delegate_to_<nom>
	// exposé à l'orchestrateur : sans elle, le modèle ne connaît du délégué
	// que son nom, et un petit modèle conclut souvent qu'il ne sait pas
	// faire (« je n'ai pas accès à Internet ») au lieu de déléguer.
	//
	// Sans effet sur un orchestrateur, qui n'est le délégué de personne.
	Description string `yaml:"description"`
	// Le modèle de l'agent ne se déclare plus ici : chaque agent est un
	// rôle de l'écran des modèles, réglé en ligne — par défaut d'instance
	// et, éventuellement, par organisation.
	SystemPrompt SystemPrompt `yaml:"system_prompt"`
	Delegates    []string     `yaml:"delegates"`
	Memory       AgentMemory  `yaml:"memory"`
	// Reminders expose à cet agent les outils de rappels ponctuels
	// (create_reminder, list_reminders, cancel_reminder). L'autorisation
	// effective de chaque appel reste vérifiée par principal via les
	// permissions reminder.<scope>.<action> (identities.roles).
	Reminders bool `yaml:"reminders"`
	// ProfileLink expose open_profile_link : l'agent peut ouvrir à son
	// interlocuteur un lien temporaire vers sa page de profil (socle SaaS).
	ProfileLink bool `yaml:"profile_link"`
	// ImageGeneration donne à ce spécialiste l'outil generate_image.
	// L'image produite est jointe à la réponse et remonte jusqu'au canal.
	// Spécialiste seulement, comme les serveurs MCP. Le MODÈLE qui dessine
	// se règle en ligne : rôle « image:<agent> » de l'écran des modèles.
	ImageGeneration bool `yaml:"image_generation"`
	// ScheduledTasks expose à cet agent les outils de tâches planifiées
	// (schedule_task, list_scheduled_tasks, cancel_scheduled_task). Une
	// tâche fait TRAVAILLER l'agent à l'échéance, là où un rappel ne fait
	// que délivrer un texte : c'est un pouvoir distinct, accordé
	// séparément, et soumis aux permissions task.<portée>.<action>.
	//
	// L'exécution est en lecture seule stricte : les actions sensibles
	// proposées pendant un tour planifié sont ignorées, jamais exécutées
	// sans humain devant l'écran.
	ScheduledTasks bool `yaml:"scheduled_tasks"`
	// RequiresAttachments déclare un spécialiste inutile sans pièce jointe
	// (un lecteur d'images, un transcripteur de documents). L'orchestrateur
	// refuse alors de le solliciter quand le tour n'en porte aucune, au
	// lieu de le laisser répondre à vide — un modèle multimodal sans image
	// invente ce qu'il aurait vu.
	//
	// Sans effet sur un orchestrateur, qui n'est le délégué de personne.
	RequiresAttachments bool        `yaml:"requires_attachments"`
	MCPServers          []string    `yaml:"mcp_servers"`
	Capabilities        []string    `yaml:"capabilities"`
	Limits              AgentLimits `yaml:"limits"`
}

// ImageClient décrit un client de génération d'images. Contrairement aux
// llm_clients, base_url est facultative : chaque provider embarque l'URL de
// son service, et openrouter/minimax ne sont pas des services « compatibles
// OpenAI » interchangeables où l'omission enverrait la clé ailleurs.
type ImageClient struct {
	// Provider vaut "openai", "openrouter" ou "minimax".
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	APIKey   string `yaml:"api_key"`
	BaseURL  string `yaml:"base_url"`
}

// SystemPrompt décrit la source du system prompt d'un agent : soit un
// fichier, soit un contenu direct. Le contenu chargé est disponible via
// Content après Load.
type SystemPrompt struct {
	File    string `yaml:"file"`
	Inline  string `yaml:"inline"`
	Content string `yaml:"-"`
	// OrgOverrides remplace la personnalité de l'agent pour les canaux
	// d'une organisation donnée (clé : organizations[].id). Seule la
	// personnalité change : les règles invariantes, les capacités et les
	// règles d'honnêteté restent celles de l'agent. Un override ne porte
	// jamais ses propres org_overrides (refusé à la validation).
	OrgOverrides map[string]SystemPrompt `yaml:"org_overrides"`
}

// AgentMemory décrit les capacités mémoire accordées à un agent.
type AgentMemory struct {
	Search   bool `yaml:"search"`
	Remember bool `yaml:"remember"`
	Forget   bool `yaml:"forget"`
	// History expose search_conversation_history : la recherche dans les
	// fragments verbatim conservés par la mémoire épisodique
	// (conversation.compaction.record_episodes).
	History bool `yaml:"history"`
	// Recall active le rappel automatique : à chaque tour, les souvenirs
	// les plus pertinents pour le message entrant sont injectés dans le
	// contexte, sans attendre un appel explicite à search_memory.
	Recall bool `yaml:"recall"`
}

// AgentLimits décrit les limites d'exécution d'un agent.
type AgentLimits struct {
	MaxSequentialToolCalls int      `yaml:"max_sequential_tool_calls"`
	MaxActionsPerTurn      int      `yaml:"max_actions_per_turn"`
	ToolTimeout            Duration `yaml:"tool_timeout"`
	MaxToolResultBytes     ByteSize `yaml:"max_tool_result_bytes"`
	MaxToolContextBytes    ByteSize `yaml:"max_tool_context_bytes"`
	// JudgeAttempts plafonne les appels au juge pour un tour (voir
	// internal/agent/judge.go). Le juge relit les réponses écrites sans
	// aucun appel d'outil ; s'il ne se prononce pas après ces essais, le
	// tour est abandonné plutôt que de rendre un texte invérifiable.
	// Absent ou nul : défaut de l'application (3). Sans effet sur un agent
	// qui n'orchestre pas.
	JudgeAttempts int `yaml:"judge_attempts"`
}

// MCPServer décrit un serveur MCP accessible aux agents, et la façon dont
// l'application traite ses outils.
//
// Tout ce qui distingue un serveur d'agenda d'un serveur de recherche est
// déclaré ici : l'application ne connaît aucun service par son nom. Brancher
// un nouveau domaine (météo, CRM, domotique) se fait entièrement en
// configuration.
type MCPServer struct {
	Transport string            `yaml:"transport"`
	URL       string            `yaml:"url"`
	Headers   map[string]string `yaml:"headers"`
	// Command est la commande lancée pour un serveur en transport "stdio" :
	// exécutable puis arguments, sans interprétation par un shell. Les
	// arguments et les valeurs de Env peuvent contenir des patrons {{nom}},
	// résolus par principal via identities.principals[].mcp.<serveur>.values
	// — c'est ce qui permet à chacun d'atteindre SON service (hôte, compte)
	// derrière une commande commune. Requis quand Transport vaut "stdio",
	// sans effet sinon.
	Command []string `yaml:"command"`
	// Env est l'environnement ajouté au processus du serveur stdio, en plus
	// de celui du worker. Les secrets (mots de passe, jetons) passent par
	// ici, JAMAIS par Command : les arguments d'un processus sont lisibles
	// par tout processus local (/proc/<pid>/cmdline), son environnement ne
	// l'est que par le même utilisateur système.
	Env map[string]string `yaml:"env"`
	// Resource, si déclarée, fait injecter par l'application un identifiant
	// de ressource dans chaque appel d'outil. Absente, les arguments passent
	// tels quels.
	Resource *MCPResource `yaml:"resource"`
	// PermissionDomain est le premier segment des permissions exigées par ce
	// serveur ("calendar" produit calendar.<portée>.write). Requis dès que
	// Tools.ConfirmWrites est vrai.
	PermissionDomain string `yaml:"permission_domain"`
	// Tools décrit comment classer et encadrer les outils du serveur.
	Tools MCPTools `yaml:"tools"`
}

// MCPResource associe un serveur MCP à une ressource déclarée par canal.
//
// Key est la clé lue dans channels[].resources. Parameter est le nom sous
// lequel le serveur attend l'identifiant. Une valeur fournie par le modèle
// sous ce nom est toujours écartée : l'application résout la ressource depuis
// la portée de la conversation, jamais depuis les arguments du modèle
// (plan de conception, §9.2).
type MCPResource struct {
	Key       string `yaml:"key"`
	Parameter string `yaml:"parameter"`
}

// MCPTools décrit le traitement appliqué aux outils d'un serveur.
type MCPTools struct {
	// ConfirmWrites transforme les outils d'écriture en actions soumises à
	// la confirmation de l'utilisateur, au lieu de les exécuter. Faux par
	// défaut : tous les outils s'exécutent directement, ce qui convient à un
	// service en lecture seule comme une recherche web.
	ConfirmWrites bool `yaml:"confirm_writes"`
	// ReadPrefixes énumère les préfixes de nom identifiant un outil de
	// lecture, exécuté directement. Tout outil hors de cette liste est
	// considéré en écriture lorsque ConfirmWrites est vrai.
	//
	// Le protocole MCP expose bien une annotation readOnlyHint, mais elle
	// n'est pas transportée jusqu'aux outils par la bibliothèque genai : la
	// classification par nom est le seul signal disponible côté application.
	// Liste vide avec ConfirmWrites vrai : tous les outils exigent une
	// confirmation, position prudente par défaut.
	ReadPrefixes []string `yaml:"read_prefixes"`
	// DedupeWrites écarte deux actions d'écriture strictement identiques
	// proposées dans le même tour. Utile pour un service où une double
	// création est un incident, inutile ailleurs.
	DedupeWrites bool `yaml:"dedupe_writes"`
	// RequireRFC3339 énumère les paramètres qui doivent être des dates
	// RFC3339 avec fuseau explicite. Un paramètre ambigu fait échouer la
	// proposition avec un message clair, plutôt que d'enregistrer une action
	// dont personne ne sait à quelle heure elle aura lieu.
	RequireRFC3339 []string `yaml:"require_rfc3339"`
	// TrustReadOnlyHint autorise l'annotation readOnlyHint du serveur à
	// classer un outil en LECTURE, ce qui le dispense de confirmation.
	//
	// Faux par défaut, et ce défaut compte. L'annotation est déclarative :
	// c'est le serveur qui l'affirme, rien ne la vérifie. Un serveur mal
	// écrit ou compromis qui annoncerait une suppression comme lecture
	// contournerait la confirmation, c'est-à-dire la garantie centrale du
	// système. Ne l'activer que pour un serveur dont on maîtrise le code.
	//
	// Indépendamment de ce drapeau, l'annotation est toujours écoutée dans le
	// sens qui RESTREINT : un outil annoté comme écrivant exige une
	// confirmation même si son nom commence par un préfixe de lecture. Croire
	// un serveur qui se déclare dangereux ne coûte rien.
	TrustReadOnlyHint bool `yaml:"trust_read_only_hint"`
}

// Memory décrit la configuration du système de mémoire (Amoxtli).
type Memory struct {
	Store         MemoryStore         `yaml:"store"`
	Indexes       []MemoryIndex       `yaml:"indexes"`
	Retrieval     MemoryRetrieval     `yaml:"retrieval"`
	Policies      MemoryPolicies      `yaml:"policies"`
	Consolidation MemoryConsolidation `yaml:"consolidation"`
}

// MemoryConsolidation décrit la réorganisation périodique des souvenirs
// (internal/consolidation) : fusion des redondances et oubli des faits
// périmés, portée par portée, pour que la mémoire ne s'accumule pas sans
// limite. Désactivée par défaut.
type MemoryConsolidation struct {
	Enabled bool `yaml:"enabled"`
	// Le modèle qui produit les plans de consolidation se règle en
	// ligne : rôle « consolidation » de l'écran des modèles.
	// Cron est l'expression cron standard (5 champs, heure locale du
	// serveur) de la cadence de consolidation. Vide applique le défaut
	// ("40 4 * * *", chaque nuit vers 4h40).
	Cron string `yaml:"cron"`
	// MinMemories est le nombre de souvenirs à partir duquel une portée est
	// consolidée : en dessous, elle est laissée intacte (rien à gagner, et
	// aucun appel LLM). 0 applique le défaut (10).
	MinMemories int `yaml:"min_memories"`
	// Reflection décrit la réflexion épisodique adossée à la même passe
	// nocturne.
	Reflection MemoryReflection `yaml:"reflection"`
}

// MemoryReflection décrit la réflexion épisodique
// (internal/consolidation/reflection.go) : une seconde phase de la passe de
// consolidation qui relit les épisodes verbatim récents, portée par portée,
// pour en dégager des motifs récurrents jamais énoncés explicitement
// (habitudes, préférences implicites), mémorisés comme souvenirs
// sémantiques d'origine "episode_reflection". Les épisodes sont lus, jamais
// modifiés. Désactivée par défaut : c'est la production de souvenirs la
// plus spéculative, et la plus coûteuse en tokens (du verbatim, pas des
// faits condensés).
type MemoryReflection struct {
	Enabled bool `yaml:"enabled"`
	// MinEpisodes est le nombre d'épisodes nouveaux à partir duquel une
	// portée est soumise à réflexion : en dessous, elle attend d'accumuler
	// davantage de matière (un motif exige plusieurs occurrences), sans
	// appel LLM. 0 applique le défaut (5).
	MinEpisodes int `yaml:"min_episodes"`
	// RetentionDays est l'âge, en jours, au-delà duquel un épisode DÉJÀ
	// COUVERT par une réflexion réussie est purgé — consolider avant
	// d'oublier. 0 (défaut) désactive toute purge : les épisodes sont
	// conservés sans limite.
	RetentionDays int `yaml:"retention_days"`
}

// MemoryStore décrit le stockage principal de la mémoire.
type MemoryStore struct {
	Driver string `yaml:"driver"`
	Path   string `yaml:"path"`
}

// MemoryIndex décrit un index de recherche mémoire. Type vaut "bleve"
// (plein texte) ou "sqlitevec" (sémantique, vecteurs) ; la recherche
// hybride s'obtient en déclarant les deux, pondérés par Weight.
type MemoryIndex struct {
	ID     string  `yaml:"id"`
	Type   string  `yaml:"type"`
	Path   string  `yaml:"path"`
	Weight float64 `yaml:"weight"`
	// Le client d'embeddings d'un index "sqlitevec" se règle en ligne :
	// rôle « embeddings:<id> » de l'écran des modèles, VERROUILLÉ après le
	// premier démarrage réussi (sentinelle à côté du fichier d'index) —
	// changer de modèle rendrait les vecteurs déjà écrits incomparables.
}

// MemoryRetrieval décrit le comportement de la recherche mémoire.
type MemoryRetrieval struct {
	// Profile choisit le compromis coût/qualité de la recherche, calqué sur
	// les profils amoxtli : "fast" (défaut, aucun appel LLM à la recherche)
	// ou "balanced" (HyDE : un appel de complétion par requête distincte,
	// meilleure recherche sémantique).
	Profile string `yaml:"profile"`
	// Le modèle de l'étape HyDE (profil "balanced") se règle en ligne :
	// rôle « retrieval » de l'écran des modèles. Un modèle économique
	// suffit.
}

// MemoryPolicies décrit les règles de propagation entre portées mémoire.
type MemoryPolicies struct {
	PrivateCanWriteOrg    bool `yaml:"private_can_write_org"`
	OrgReadableByChildren bool `yaml:"org_readable_by_children"`
}

// Identities décrit les rôles et principaux connus de l'instance.
type Identities struct {
	Roles      map[string]Role `yaml:"roles"`
	Principals []Principal     `yaml:"principals"`
}

// Role décrit un rôle et ses permissions.
type Role struct {
	Permissions []string `yaml:"permissions"`
}

// PrincipalKind énumère les types de principaux supportés.
type PrincipalKind string

const (
	PrincipalKindHuman   PrincipalKind = "human"
	PrincipalKindService PrincipalKind = "service"
)

// Principal décrit un principal (humain ou service) connu de l'instance.
type Principal struct {
	ID          string        `yaml:"id"`
	Kind        PrincipalKind `yaml:"kind"`
	DisplayName string        `yaml:"display_name"`
	Roles       []string      `yaml:"roles"`
	// Locale surcharge default_locale pour ce principal. Vide = le défaut
	// de l'instance. Les membres rattachés en ligne portent la leur en base
	// (members.locale) plutôt qu'ici.
	Locale string `yaml:"locale"`
	// Orgs liste les organisations auxquelles ce principal appartient.
	// Facultatif tant que l'instance n'en sert qu'une, obligatoire au-delà :
	// c'est cette liste qui empêche un collègue d'atteindre la mémoire de la
	// famille, et un oubli ne doit pas se traduire par un accès aux deux.
	Orgs []string `yaml:"orgs"`
	// MCP surcharge, par nom de serveur, la façon dont ce principal s'y
	// connecte : son propre jeton, éventuellement sa propre URL. C'est ce qui
	// permet à chacun d'atteindre SON agenda ou SA liste de tâches sur un
	// service qui authentifie l'utilisateur final.
	//
	// Une surcharge change la clé de session MCP (voir internal/mcp) : deux
	// principaux ne partagent jamais une connexion authentifiée, même dans un
	// canal de groupe.
	MCP map[string]MCPOverride `yaml:"mcp"`
}

// MCPOverride décrit la connexion d'un principal à un serveur MCP donné.
//
// Serveur http : URL vide conserve celle du serveur ; les en-têtes déclarés
// ici s'ajoutent à ceux du serveur et l'emportent en cas de même nom, ce qui
// permet de ne surcharger que l'autorisation sans réécrire le reste.
//
// Serveur stdio : Values fournit les valeurs des patrons {{nom}} déclarés
// dans Command et Env du serveur. Un principal sans surcharge pour un
// serveur à patrons n'y a simplement pas accès (l'outil est indisponible
// pour lui) : il n'existe AUCUN repli sur les valeurs d'un autre principal.
type MCPOverride struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Values  map[string]string `yaml:"values"`
}

// Origin associe une identité externe (fournisseur + identifiant externe) à
// un principal connu.
type Origin struct {
	Provider       string `yaml:"provider"`
	ExternalUserID string `yaml:"external_user_id"`
	PrincipalID    string `yaml:"principal_id"`
}

// ChannelKind énumère les types de canaux supportés.
type ChannelKind string

const (
	ChannelKindPrivate ChannelKind = "private"
	ChannelKindGroup   ChannelKind = "group"
)

// Scope énumère les portées supportées.
type Scope string

const (
	ScopePersonal Scope = "personal"
	ScopeGroup    Scope = "group"
	ScopeOrg      Scope = "org"
)

// Channel décrit un canal de conversation (privé ou groupe).
type Channel struct {
	Provider    string            `yaml:"provider"`
	ChannelID   string            `yaml:"channel_id"`
	DisplayName string            `yaml:"display_name"`
	Kind        ChannelKind       `yaml:"kind"`
	OrgID       string            `yaml:"org_id"`
	Scope       Scope             `yaml:"scope"`
	ScopeID     string            `yaml:"scope_id"`
	Activation  string            `yaml:"activation"`
	Members     []string          `yaml:"members"`
	PrincipalID string            `yaml:"principal_id"`
	Resources   map[string]string `yaml:"resources"`
}

// Schedule décrit un déclenchement planifié.
type Schedule struct {
	ID          string              `yaml:"id"`
	Enabled     bool                `yaml:"enabled"`
	Schedule    ScheduleCron        `yaml:"schedule"`
	Execution   ScheduleExecution   `yaml:"execution"`
	Delivery    ScheduleDelivery    `yaml:"delivery"`
	Concurrency ScheduleConcurrency `yaml:"concurrency"`
}

// ScheduleCron décrit l'expression cron et le fuseau horaire d'un
// déclenchement planifié.
type ScheduleCron struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

// ActionsPolicy énumère les politiques d'exécution des actions supportées.
type ActionsPolicy string

const (
	ActionsPolicyReadOnly            ActionsPolicy = "read_only"
	ActionsPolicyRequireConfirmation ActionsPolicy = "require_confirmation"
)

// ScheduleActions décrit la politique d'actions d'un déclenchement planifié.
type ScheduleActions struct {
	Policy ActionsPolicy `yaml:"policy"`
}

// ScheduleExecution décrit le contexte d'exécution d'un déclenchement
// planifié.
type ScheduleExecution struct {
	PrincipalID string          `yaml:"principal_id"`
	OrgID       string          `yaml:"org_id"`
	Scope       Scope           `yaml:"scope"`
	ScopeID     string          `yaml:"scope_id"`
	Agent       string          `yaml:"agent"`
	Prompt      string          `yaml:"prompt"`
	Actions     ScheduleActions `yaml:"actions"`
}

// DeliveryMode énumère les modes de livraison supportés.
type DeliveryMode string

const (
	DeliveryModeAlways    DeliveryMode = "always"
	DeliveryModeOnContent DeliveryMode = "on_content"
	DeliveryModeOnFailure DeliveryMode = "on_failure"
)

// ScheduleDelivery décrit la livraison du résultat d'un déclenchement
// planifié.
type ScheduleDelivery struct {
	Provider  string       `yaml:"provider"`
	ChannelID string       `yaml:"channel_id"`
	Mode      DeliveryMode `yaml:"mode"`
}

// ConcurrencyPolicy énumère les politiques de concurrence supportées.
type ConcurrencyPolicy string

const (
	ConcurrencyPolicyForbid ConcurrencyPolicy = "forbid"
	ConcurrencyPolicyAllow  ConcurrencyPolicy = "allow"
)

// ScheduleConcurrency décrit la politique de concurrence d'un déclenchement
// planifié.
type ScheduleConcurrency struct {
	Policy  ConcurrencyPolicy `yaml:"policy"`
	Timeout Duration          `yaml:"timeout"`
}
