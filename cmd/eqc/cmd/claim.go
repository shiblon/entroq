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
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var (
	flagClaimQueues  []string
	flagClaimTry     bool
	flagDurationSecs int
)

func init() {
	rootCmd.AddCommand(claimCmd)

	claimCmd.Flags().StringArrayVarP(&flagClaimQueues, "queue", "q", nil, "Queue to claim from. Required, can be repeated to claim from one of several queues.")
	claimCmd.MarkFlagRequired("queue")

	claimCmd.Flags().BoolVar(&flagClaimTry, "try", false, "Use non-blocking try-claim.")
	claimCmd.Flags().IntVarP(&flagDurationSecs, "duration", "d", 30, "Claim for this many seconds.")
}

// claimCmd represents the claim command
var claimCmd = &cobra.Command{
	Use:   "claim",
	Short: "Claim a task from a queue (or queues).",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(flagClaimQueues) == 0 {
			return errors.New("No claim queues specified")
		}

		ctx := context.Background()

		var (
			task *entroq.Task
			err  error
		)

		duration := time.Duration(flagDurationSecs) * time.Second

		if flagClaimTry {
			if task, err = eq.TryClaim(ctx, entroq.From(flagClaimQueues...), entroq.ClaimFor(duration)); err != nil {
				return err
			}
		} else {
			if task, err = eq.Claim(ctx, entroq.From(flagClaimQueues...), entroq.ClaimFor(duration)); err != nil {
				return err
			}
		}

		if task == nil {
			log.Print("No task")
			return nil
		}
		fmt.Println(mustTaskString(task))
		return nil
	},
}
