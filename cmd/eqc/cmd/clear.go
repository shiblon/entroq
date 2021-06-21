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
	"log"
	"time"

	"entrogo.com/entroq"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	flagClearQueue string
	flagClearForce bool
)

func init() {
	rootCmd.AddCommand(clearCmd)

	clearCmd.Flags().StringVarP(&flagClearQueue, "queue", "q", "", "Task Queue to clear.")
	clearCmd.MarkFlagRequired("queue")

	clearCmd.Flags().BoolVarP(&flagClearForce, "force", "f", false, "CAREFUL: forces deletion even of claimed tasks. Spoofs claimant to delete even if task is claimed.")
}

// clearCmd represents the clear command
var clearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Remove all tasks from a queue.",
	Run: func(cmd *cobra.Command, args []string) {
		ctx := context.Background()
		if flagClearForce {
			for {
				empty, err := eq.QueuesEmpty(ctx, entroq.MatchExact(flagClearQueue))
				if err != nil {
					log.Fatalf("Failed to check queue emptiness %q: %v", flagClearQueue, err)
				}
				if empty {
					break
				}
				tasks, err := eq.Tasks(ctx, flagClearQueue, entroq.LimitTasks(100), entroq.OmitValues())
				if err != nil {
					log.Fatalf("Failed to get tasks from %q: %v", flagClearQueue, err)
				}

				byClaimant := make(map[uuid.UUID][]entroq.ModifyArg)
				for _, t := range tasks {
					byClaimant[t.Claimant] = append(byClaimant[t.Claimant], t.AsDeletion())
				}
				for cid, args := range byClaimant {
					forceArgs := append(args, entroq.ModifyAs(cid))
					if _, _, err := eq.Modify(ctx, forceArgs...); err != nil {
						log.Fatalf("Force clear failed: %v", err)
					}
				}
			}
			return
		}

		for {
			empty, err := eq.QueuesEmpty(ctx, entroq.MatchExact(flagClearQueue))
			if err != nil {
				log.Fatalf("Error getting empty status for %q: %v", flagClearQueue, err)
			}
			if empty {
				break
			}
			tctx, _ := context.WithTimeout(ctx, 10*time.Second)
			task, err := eq.Claim(tctx, entroq.From(flagClearQueue))
			if err != nil {
				if entroq.IsTimeout(err) {
					continue
				}
				log.Fatalf("Error claiming: %v", err)
			}
			if _, _, err := eq.Modify(ctx, task.AsDeletion()); err != nil {
				log.Fatalf("Error deleting: %v", err)
			}
		}
	},
}
