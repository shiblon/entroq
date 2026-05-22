package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var (
	flagNsPrefix []string
	flagNsExact  []string
	flagNsLimit  int
	flagNsJSON   bool
)

func init() {
	rootCmd.AddCommand(nsCmd)

	nsCmd.Flags().StringArrayVarP(&flagNsPrefix, "prefix", "p", nil, "Prefix match, if any. May be specified more than once.")
	nsCmd.Flags().StringArrayVarP(&flagNsExact, "namespace", "n", nil, "Exact match, if any. May be specified more than once.")
	nsCmd.Flags().IntVarP(&flagNsLimit, "limit", "l", 0, "Limit number of namespaces returned.")
	nsCmd.Flags().BoolVarP(&flagNsJSON, "json", "j", false, "Output as JSON instead of a table.")
}

var nsCmd = &cobra.Command{
	Use:     "ns",
	Aliases: []string{"namespaces"},
	Short:   "Return a list of (matching, if specified) doc namespaces",
	RunE: func(cmd *cobra.Command, args []string) error {
		var opts []entroq.QueuesOpt
		for _, p := range flagNsPrefix {
			opts = append(opts, entroq.MatchPrefix(p))
		}
		for _, e := range flagNsExact {
			opts = append(opts, entroq.MatchExact(e))
		}
		if lim := flagNsLimit; lim > 0 {
			opts = append(opts, entroq.WithLimit(lim))
		}

		ns, err := eq.NamespaceStats(context.Background(), opts...)
		if err != nil {
			return fmt.Errorf("get namespaces: %w", err)
		}

		if flagNsJSON {
			b, err := json.Marshal(ns)
			if err != nil {
				return fmt.Errorf("namespaces json: %w", err)
			}
			fmt.Println(string(b))
			return nil
		}

		var names []string
		for k := range ns {
			names = append(names, k)
		}
		sort.Strings(names)

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAMESPACE\tSIZE\tCLAIMED")
		for _, name := range names {
			n := ns[name]
			fmt.Fprintf(tw, "%s\t%d\t%d\n", n.Name, n.Size, n.Claimed)
		}
		tw.Flush()
		return nil
	},
}
