// Command automata est le point d'entrée de l'application Automata.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bornholm/automata/internal/config"
	"github.com/bornholm/automata/internal/persistence"
	"github.com/bornholm/automata/internal/registry"
	"github.com/bornholm/automata/internal/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Le même logger devient celui par défaut. Sans cela, tout composant
	// qui appelle slog au niveau paquet — le gestionnaire de plugins, et
	// donc les lignes remontées des sous-processus — écrivait en TEXTE, à
	// côté du JSON de tout le reste. Deux formats dans un même flux
	// n'étaient filtrables par aucun outil : c'est ce qui a rendu les
	// journaux des plugins pénibles à exploiter, une fois qu'ils y sont
	// enfin arrivés.
	slog.SetDefault(logger)

	if err := newRootCommand(logger).Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCommand construit la commande racine automata. Sans sous-commande,
// elle conserve le comportement historique : chargement de la configuration
// puis démarrage du registry avec gestion de SIGINT/SIGTERM.
//
// Le drapeau -config suit la convention Go (préfixe simple tiret), comme
// pour "config validate" : le parsing des drapeaux de cobra est donc
// désactivé et délégué au paquet flag standard.
//
// Args: cobra.ArbitraryArgs est nécessaire ici : le drapeau -config étant en
// simple tiret et multi-caractères, cobra ne le reconnaît pas comme un
// drapeau consommant une valeur lors de la résolution de commande
// (stripFlags/Find, package cobra) puisque aucun drapeau n'est enregistré
// sur cette commande (DisableFlagParsing délègue tout au paquet flag
// standard dans RunE). Sans cette annotation, la validation par défaut de
// cobra (legacyArgs) traite la VALEUR du drapeau (ex. "/config/config.yaml")
// comme une tentative de sous-commande inconnue et Execute() échoue avec
// "unknown command", avant même l'appel de RunE — silencieusement, à cause
// de SilenceErrors : c'était un défaut latent depuis les phases précédentes,
// jamais exercé faute de test exécutant le binaire compilé sans
// sous-commande (voir Phase 22, packaging, où l'image Docker invoque
// justement "automata -config ...").
func newRootCommand(logger *slog.Logger) *cobra.Command {
	root := &cobra.Command{
		Use:                "automata",
		Short:              "Automata assemble les services applicatifs de l'assistant",
		SilenceUsage:       true,
		SilenceErrors:      true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("automata", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			if err := run(logger, *configPath); err != nil {
				logger.Error("automata exited with error", "error", err)
				return err
			}

			return nil
		},
	}

	root.AddCommand(newConfigCommand())
	root.AddCommand(newHealthcheckCommand())
	root.AddCommand(newMemoryCommand(logger))
	root.AddCommand(newAdminCommand())
	root.AddCommand(newUsageCommand())
	root.AddCommand(newVersionCommand())
	root.AddCommand(newStorageCommand())
	root.AddCommand(newWebCommand())

	return root
}

func run(logger *slog.Logger, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("chargement de la configuration: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return registry.Run(ctx, logger, cfg)
}

// newConfigCommand construit la commande "config" et sa sous-commande
// "validate".
func newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Gestion de la configuration Automata",
	}

	cmd.AddCommand(newConfigInitCommand())
	cmd.AddCommand(newConfigValidateCommand())

	return cmd
}

// newConfigInitCommand construit la sous-commande "config init" : un
// entretien en ligne de commande qui produit une configuration complète et
// le fichier d'environnement correspondant.
//
// Elle n'écrit aucun secret : les valeurs sensibles deviennent des références
// d'environnement, listées dans le fichier généré à côté.
func newConfigInitCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "init",
		Short:              "Génère un fichier de configuration en répondant à quelques questions",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("init", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			output := fs.String("output", "config/config.yaml", "chemin du fichier de configuration à écrire")
			envOutput := fs.String("env-output", "", "chemin du fichier d'environnement à écrire (défaut : <output>.env)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			envPath := *envOutput
			if envPath == "" {
				envPath = strings.TrimSuffix(*output, filepath.Ext(*output)) + ".env"
			}

			configYAML, envExample, err := runConfigInit(cmd.InOrStdin(), cmd.OutOrStdout())
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
				return errSilent
			}

			// Les deux fichiers sont écrits seulement une fois l'entretien
			// terminé : une interruption en cours de route ne laisse rien
			// derrière elle.
			if err := writeIfAbsent(*output, configYAML); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
				return errSilent
			}

			if err := writeIfAbsent(envPath, envExample); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
				return errSilent
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "\nConfiguration écrite dans %s\n", *output)
			fmt.Fprintf(out, "Variables à renseigner dans %s\n", envPath)
			fmt.Fprintln(out, "\nEnsuite :")
			fmt.Fprintf(out, "  1. renseigner les variables, puis les charger dans l'environnement\n")
			fmt.Fprintf(out, "  2. automata config validate -config %s\n", *output)
			fmt.Fprintf(out, "  3. automata -config %s\n", *output)

			return nil
		},
	}

	return cmd
}

// newConfigValidateCommand construit la sous-commande "config validate".
//
// Le drapeau -config suit la convention Go (préfixe simple tiret) plutôt que
// la convention pflag (double tiret) : le parsing des drapeaux de cobra est
// donc désactivé pour cette commande et délégué au paquet flag standard.
func newConfigValidateCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "validate",
		Short:              "Charge et valide intégralement la configuration YAML",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("validate", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			orgIDs := make([]string, 0, len(cfg.AllOrganizations()))
			for _, org := range cfg.AllOrganizations() {
				orgIDs = append(orgIDs, org.ID)
			}

			// Aucune organisation configurée est un cas légitime : elles
			// sont alors créées depuis l'administration web. Le dire vaut
			// mieux qu'afficher une liste vide.
			orgSummary := "organisation(s) " + strings.Join(orgIDs, ", ")
			if len(orgIDs) == 0 {
				orgSummary = "organisations créées depuis l'administration"
			}

			fmt.Fprintf(cmd.OutOrStdout(), "configuration valide: %s (%s, %d agent(s))\n", *configPath, orgSummary, len(cfg.Agents))

			return nil
		},
	}

	return cmd
}

// defaultHealthcheckAddr est l'adresse interrogée par "automata healthcheck"
// en l'absence de drapeau -addr. Elle correspond à la valeur documentée pour
// observability.addr dans docs/deployment.md ; un déploiement qui choisit une
// autre adresse doit passer -addr explicitement.
const defaultHealthcheckAddr = "127.0.0.1:9090"

// newHealthcheckCommand construit la commande "healthcheck" (le plan de conception Phase
// 22, point 5).
//
// Elle existe pour rendre la sonde de santé exécutable DEPUIS le conteneur :
// l'image finale est une distroless "static", sans shell ni client HTTP, si
// bien qu'aucune directive HEALTHCHECK ne pouvait y être définie. Le binaire
// applicatif, lui, est présent — il fournit donc lui-même le client HTTP
// minimal dont la sonde a besoin.
//
// Le code de sortie est le seul contrat : 0 si le service est prêt, 1 dans
// tous les autres cas (service non prêt, injoignable, délai dépassé).
func newHealthcheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "healthcheck",
		Short:              "Vérifie que le service local est prêt (code de sortie 0 ou 1)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			addr := fs.String("addr", defaultHealthcheckAddr, "adresse du serveur d'observabilité à interroger")
			// Le serveur d'observabilité est facultatif : là où il est
			// désactivé, la sonde du service web (-addr <hôte>:5000 -path
			// /healthz) rend le même service, et interroge la base en plus.
			path := fs.String("path", "/healthz/ready", "chemin de la sonde (/healthz pour celle du serveur web)")
			timeout := fs.Duration("timeout", 3*time.Second, "délai maximal de la sonde")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			url := "http://" + *addr + *path

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "sonde invalide: %v\n", err)
				return errSilent
			}

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "service injoignable sur %s: %v\n", *addr, err)
				return errSilent
			}
			defer func() {
				_ = resp.Body.Close()
			}()

			if resp.StatusCode != http.StatusOK {
				fmt.Fprintf(cmd.ErrOrStderr(), "service non prêt sur %s (code %d)\n", *addr, resp.StatusCode)
				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// newMemoryCommand construit la commande "memory" et sa sous-commande
// "reindex" (plan de conception, §8.6, Phase 10).
func newMemoryCommand(logger *slog.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Gestion de la mémoire persistante Amoxtli",
	}

	cmd.AddCommand(newMemoryReindexCommand(logger))

	return cmd
}

// newMemoryReindexCommand construit la sous-commande "memory reindex" : elle
// charge la configuration, construit le codex amoxtli décrit par
// cfg.Memory, et déclenche une réindexation complète.
//
// Le drapeau -config suit la convention Go (préfixe simple tiret), comme
// pour "config validate".
func newMemoryReindexCommand(logger *slog.Logger) *cobra.Command {
	cmd := &cobra.Command{
		Use:                "reindex",
		Short:              "Réindexe intégralement la mémoire à partir du store",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			if err := registry.MemoryReindex(cmd.Context(), logger, cfg); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "réindexation échouée:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			fmt.Fprintln(cmd.OutOrStdout(), "réindexation terminée avec succès")

			return nil
		},
	}

	return cmd
}

// newUsageRepriceCommand construit "usage reprice" : elle estime
// rétroactivement le coût des appels que le fournisseur n'a pas facturés,
// à partir de la grille tarifaire. Sans elle, ces appels resteraient à
// zéro et échapperaient définitivement à la facturation.
func newUsageRepriceCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "reprice",
		Short:              "Estime le coût des appels enregistrés sans coût rapporté",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("reprice", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			if err := registry.UsageReprice(cmd.Context(), cfg, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "estimation échouée:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// newStorageCommand construit la commande "storage" et sa sous-commande
// "encrypt" (chiffrement des contenus déjà écrits en clair).
func newStorageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Opérations sur la base applicative",
	}

	cmd.AddCommand(newStorageEncryptCommand())

	return cmd
}

func newStorageEncryptCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "encrypt",
		Short:              "Chiffre les contenus déjà enregistrés en clair",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("encrypt", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			if err := registry.StorageEncrypt(cmd.Context(), cfg, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "chiffrement échoué:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// newWebCommand construit la commande "web" et ses sous-commandes
// "hash-password" (hachage bcrypt du mot de passe opérateur) et
// "bootstrap" (import des tenants de la configuration vers la base).
func newWebCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "web",
		Short: "Outils du serveur web d'administration et de profil",
	}

	cmd.AddCommand(newWebHashPasswordCommand())
	cmd.AddCommand(newWebBootstrapCommand())

	return cmd
}

// newWebHashPasswordCommand lit le mot de passe sur l'entrée standard
// (jamais en argument : il apparaîtrait dans l'historique du shell) et
// imprime le hachage bcrypt à placer dans web.admin.password_hash.
func newWebHashPasswordCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hash-password",
		Short: "Hache un mot de passe opérateur (lu sur stdin) pour web.admin.password_hash",
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "lecture du mot de passe:", err)
				return errSilent
			}

			password := strings.TrimRight(string(raw), "\r\n")
			if len(password) < 12 {
				fmt.Fprintln(cmd.ErrOrStderr(), "mot de passe trop court : 12 caractères au minimum")
				return errSilent
			}

			hash, err := web.HashPassword(password)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "hachage échoué:", err)
				return errSilent
			}

			fmt.Fprintln(cmd.OutOrStdout(), hash)

			return nil
		},
	}

	return cmd
}

// newWebBootstrapCommand importe organisations et membres de la
// configuration vers les tables du socle SaaS (idempotent).
func newWebBootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "bootstrap",
		Short:              "Importe les organisations et membres de la configuration dans la base",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			if err := registry.WebBootstrap(cmd.Context(), cfg, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "bootstrap échoué:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// newUsageCommand construit la commande "usage" et sa sous-commande
// "report" : agrégation des traces comptables d'inférence (internal/usage)
// pour la refacturation par organisation/utilisateur.
func newUsageCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Comptabilité de l'usage d'inférence LLM",
	}

	cmd.AddCommand(newUsageReportCommand())
	cmd.AddCommand(newUsageRepriceCommand())

	return cmd
}

// newUsageReportCommand construit la sous-commande "usage report" : lecture
// seule, agrégation SQL sur la période demandée. Par défaut, le mois civil
// courant, agrégé par organisation.
func newUsageReportCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "report",
		Short:              "Agrège les coûts et tokens d'inférence par organisation, utilisateur, agent ou modèle",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("report", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")
			fromFlag := fs.String("from", "", "début de période (AAAA-MM-JJ, défaut: premier jour du mois courant)")
			toFlag := fs.String("to", "", "fin de période, exclue (AAAA-MM-JJ, défaut: demain)")
			groupByFlag := fs.String("group-by", "org", "dimensions d'agrégation, séparées par des virgules: "+strings.Join(persistence.UsageGroupKeys(), ", "))

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			now := time.Now().UTC()
			from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
			if *fromFlag != "" {
				from, err = time.Parse("2006-01-02", *fromFlag)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "date -from invalide (attendu AAAA-MM-JJ): %v\n", err)
					return errSilent
				}
			}

			to := now.AddDate(0, 0, 1).Truncate(24 * time.Hour)
			if *toFlag != "" {
				to, err = time.Parse("2006-01-02", *toFlag)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "date -to invalide (attendu AAAA-MM-JJ): %v\n", err)
					return errSilent
				}
			}

			groupBy := strings.Split(*groupByFlag, ",")
			for i := range groupBy {
				groupBy[i] = strings.TrimSpace(groupBy[i])
			}

			if err := registry.UsageReport(cmd.Context(), cfg, from, to, groupBy, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "rapport d'usage échoué:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// newAdminCommand construit la commande "admin" et sa sous-commande
// "inspect" (plan de conception, §18, "ajouter une commande d'inspection
// administrative").
func newAdminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Commandes d'administration en lecture seule",
	}

	cmd.AddCommand(newAdminInspectCommand())
	cmd.AddCommand(newAdminProbeToolsCommand())

	return cmd
}

// newAdminProbeToolsCommand construit "admin probe-tools" : elle envoie au
// modèle d'un rôle le plus petit tour possible qui exige un appel d'outil,
// puis recommence avec un jeu d'outils croissant.
//
// Elle existe parce qu'un assistant qui n'appelle jamais ses outils ne
// produit AUCUNE erreur : il répond de mémoire et invente des excuses. Les
// journaux d'une instance en marche ne distinguent pas un modèle inapte
// d'un modèle noyé sous vingt outils ; cette commande, si.
func newAdminProbeToolsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "probe-tools",
		Short:              "Vérifie que le modèle d'un rôle appelle bien ses outils",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("probe-tools", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")
			role := fs.String("role", "main", "rôle à sonder (main, research, plugins, compaction...)")
			orgID := fs.String("org", "", "organisation dont la surcharge de modèle s'applique; vide: défaut de l'instance")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			if err := registry.ProbeTools(cmd.Context(), cfg, *role, *orgID, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "sonde échouée:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// newAdminInspectCommand construit la sous-commande "admin inspect" :
// lecture seule, aucune mutation. Le drapeau -kind sélectionne la vue
// ("plans" par défaut, ou "runs"), sur le même modèle que les autres
// sous-commandes de ce fichier (drapeau -config convention Go).
func newAdminInspectCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:                "inspect",
		Short:              "Inspecte l'état des plans d'actions et des exécutions planifiées (lecture seule)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
			fs.SetOutput(cmd.ErrOrStderr())

			configPath := fs.String("config", "", "chemin du fichier de configuration YAML (requis)")
			kind := fs.String("kind", "plans", "vue à inspecter: plans|runs")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			if *configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "le drapeau -config est requis")
				return errSilent
			}

			cfg, err := config.Load(*configPath)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "configuration invalide:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			if err := registry.AdminInspect(cmd.Context(), cfg, *kind, cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "inspection échouée:")
				fmt.Fprintln(cmd.ErrOrStderr(), err)

				return errSilent
			}

			return nil
		},
	}

	return cmd
}

// errSilent est retournée lorsque le message d'erreur a déjà été affiché,
// pour éviter que cobra ne le réaffiche une seconde fois.
var errSilent = errSilentType{}

type errSilentType struct{}

func (errSilentType) Error() string { return "" }
