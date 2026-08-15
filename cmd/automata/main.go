// Command automata est le point d'entrée de l'application Automata.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

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
func newRootCommand(logger *slog.Logger) *cobra.Command {
	root := &cobra.Command{
		Use:                "automata",
		Short:              "Automata assemble les services applicatifs de l'assistant",
		SilenceUsage:       true,
		SilenceErrors:      true,
		DisableFlagParsing: true,
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
	root.AddCommand(newMemoryCommand(logger))

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

	cmd.AddCommand(newConfigValidateCommand())

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

// errSilent est retournée lorsque le message d'erreur a déjà été affiché,
// pour éviter que cobra ne le réaffiche une seconde fois.
var errSilent = errSilentType{}

type errSilentType struct{}

func (errSilentType) Error() string { return "" }
