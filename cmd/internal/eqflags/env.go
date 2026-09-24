// Package eqflags provides shared command-line configuration helpers.
package eqflags

import (
	"flag"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// NewEnvironment returns a Viper instance that reads PREFIX_FLAG_NAME
// environment variables for command flags. Hyphens in flag names become
// underscores in environment keys.
func NewEnvironment(prefix string) *viper.Viper {
	v := viper.New()
	v.SetEnvPrefix(prefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	return v
}

// Apply returns a Cobra pre-run hook that applies environment and config-file
// values to every flag on the command. Explicit command-line values win.
func Apply(v *viper.Viper) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		return ApplySet(v, cmd.Flags())
	}
}

// ApplySet applies environment and config-file values to a flag set. It is
// exported primarily for commands that do not use Cobra pre-run hooks.
func ApplySet(v *viper.Viper, flags *pflag.FlagSet) error {
	var firstErr error
	flags.VisitAll(func(flag *pflag.Flag) {
		if firstErr != nil || flag.Changed || !v.IsSet(flag.Name) {
			return
		}
		value := v.GetString(flag.Name)
		if err := flags.Set(flag.Name, value); err != nil {
			firstErr = fmt.Errorf("set --%s from configuration value %q: %w", flag.Name, value, err)
		}
	})
	return firstErr
}

// ApplyGoSet is the standard-library flag equivalent of ApplySet. It supports
// long-running commands such as the Kubernetes operator that do not use Cobra.
func ApplyGoSet(v *viper.Viper, flags *flag.FlagSet) error {
	changed := make(map[string]bool)
	flags.Visit(func(flag *flag.Flag) {
		changed[flag.Name] = true
	})

	var firstErr error
	flags.VisitAll(func(flag *flag.Flag) {
		if firstErr != nil || changed[flag.Name] || !v.IsSet(flag.Name) {
			return
		}
		value := v.GetString(flag.Name)
		if err := flags.Set(flag.Name, value); err != nil {
			firstErr = fmt.Errorf("set --%s from configuration value %q: %w", flag.Name, value, err)
		}
	})
	return firstErr
}
