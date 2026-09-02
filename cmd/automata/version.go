package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bornholm/automata/internal/build"
)

// newVersionCommand affiche la version du binaire. C'est la première chose
// qu'on demande à quelqu'un qui signale un problème : elle doit se lire
// sans configuration ni base.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Affiche la version du binaire",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "automata %s\n", build.LongVersion)
			return err
		},
	}
}
