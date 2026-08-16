// Command automata est le point d'entrée de l'application Automata.
package main

import (
	"context"
	"flag"
	"fmt"
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
	"github.com/bornholm/automata/internal/registry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

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

			fmt.Fprintf(cmd.OutOrStdout(), "configuration valide: %s (organisation %q, %d agent(s))\n", *configPath, cfg.Organization.ID, len(cfg.Agents))

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

// newHealthcheckCommand construit la commande "healthcheck" (PLAN.md Phase
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
			timeout := fs.Duration("timeout", 3*time.Second, "délai maximal de la sonde")

			if err := fs.Parse(args); err != nil {
				return errSilent
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), *timeout)
			defer cancel()

			url := "http://" + *addr + "/healthz/ready"

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
// "reindex" (PLAN.md §8.6, Phase 10).
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

// newAdminCommand construit la commande "admin" et sa sous-commande
// "inspect" (PLAN.md §18, "ajouter une commande d'inspection
// administrative").
func newAdminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Commandes d'administration en lecture seule",
	}

	cmd.AddCommand(newAdminInspectCommand())

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
