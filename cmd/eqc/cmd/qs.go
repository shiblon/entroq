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

	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var (
	flagQsPrefix []string
	flagQsExact  []string
	flagQsLimit  int
)

func init() {
	rootCmd.AddCommand(qsCmd)

	qsCmd.Flags().StringArrayVarP(&flagQsPrefix, "prefix", "p", nil, "Prefix match, if any. May be specified more than once.")

	qsCmd.Flags().StringArrayVarP(&flagQsExact, "queue", "q", nil, "Exact match, if any. May be specified more than once.")

	qsCmd.Flags().IntVarP(&flagQsLimit, "limit", "n", 0, "Limit number of queues returned.")
}

// qsCmd represents the qs command
var qsCmd = &cobra.Command{
	Use:   "qs",
	Short: "Return a list of (matching, if specified) queues",
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

		b, err := json.MarshalIndent(qs, "", "\t")
		if err != nil {
			return fmt.Errorf("queues json: %w", err)
		}
		fmt.Println(string(b))

		return nil
	},
}
