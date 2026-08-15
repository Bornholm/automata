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
// elle conserve le comportement historique : démarrage du registry avec
// gestion de SIGINT/SIGTERM.
func newRootCommand(logger *slog.Logger) *cobra.Command {
	root := &cobra.Command{
		Use:           "automata",
		Short:         "Automata assemble les services applicatifs de l'assistant",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := run(logger); err != nil {
				logger.Error("automata exited with error", "error", err)
				return err
			}

			return nil
		},
	}

	root.AddCommand(newConfigCommand())

	return root
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return registry.Run(ctx, logger)
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

// errSilent est retournée lorsque le message d'erreur a déjà été affiché,
// pour éviter que cobra ne le réaffiche une seconde fois.
var errSilent = errSilentType{}

type errSilentType struct{}

func (errSilentType) Error() string { return "" }
