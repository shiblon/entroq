// Copyright © 2019 Chris Monson <shiblon@gmail.com>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
	flagQsPrefix []string
	flagQsExact  []string
	flagQsLimit  int
	flagQsJSON   bool
)

func init() {
	rootCmd.AddCommand(qsCmd)

	qsCmd.Flags().StringArrayVarP(&flagQsPrefix, "prefix", "p", nil, "Prefix match, if any. May be specified more than once.")

	qsCmd.Flags().StringArrayVarP(&flagQsExact, "queue", "q", nil, "Exact match, if any. May be specified more than once.")

	qsCmd.Flags().IntVarP(&flagQsLimit, "limit", "n", 0, "Limit number of queues returned.")

	qsCmd.Flags().BoolVarP(&flagQsJSON, "json", "j", false, "Output as JSON instead of a table.")
}

// qsCmd represents the qs command
var qsCmd = &cobra.Command{
	Use:     "qs",
	Aliases: []string{"stats"},
	Short:   "Return a list of (matching, if specified) queues",
	RunE: func(cmd *cobra.Command, args []string) error {
		var opts []entroq.QueuesOpt
		for _, p := range flagQsPrefix {
			opts = append(opts, entroq.MatchPrefix(p))
		}
		for _, e := range flagQsExact {
			opts = append(opts, entroq.MatchExact(e))
		}
		if lim := flagQsLimit; lim > 0 {
			opts = append(opts, entroq.LimitQueues(lim))
		}

		qs, err := eq.QueueStats(context.Background(), opts...)
		if err != nil {
			return fmt.Errorf("get queues: %w", err)
		}

		showJSON := flagQsJSON
		if !cmd.Flags().Changed("json") && cmd.CalledAs() == "qs" {
			showJSON = true
		}

		if showJSON {
			b, err := json.Marshal(qs)
			if err != nil {
				return fmt.Errorf("queues json: %w", err)
			}
			fmt.Println(string(b))
			return nil
		}

		// Sort by name for stability.
		var names []string
		for k := range qs {
			names = append(names, k)
		}
		sort.Strings(names)

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "QUEUE\tSIZE\tFUTURE\tAVAILABLE\tCLAIMED\tMAX CLAIMS")
		for _, name := range names {
			q := qs[name]
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\n", q.Name, q.Size, q.Future, q.Available, q.Claimed, q.MaxClaims)
		}
		tw.Flush()
		return nil
	},
}
