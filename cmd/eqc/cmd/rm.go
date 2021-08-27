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
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var (
	flagRmID      string
	flagRmRetries int
	flagRmForce   bool
)

func init() {
	rootCmd.AddCommand(rmCmd)

	rmCmd.Flags().StringVarP(&flagRmID, "task", "t", "", "Task ID to remove. Note that this will remove whatever version of the task ID it finds. Use with care. Required.")
	rmCmd.MarkFlagRequired("task")

	rmCmd.Flags().BoolVarP(&flagRmForce, "force", "f", false, "CAREFUL: forces deletion. Spoofs claimant to delete even if task is claimed.")
	rmCmd.Flags().IntVarP(&flagRmRetries, "retries", "r", 10, "Retries (in case the task is claimed)")
}

// rmCmd represents the rm command
var rmCmd = &cobra.Command{
	Use:   "rm",
	Short: "Remove a task by queue and ID.",
	Run: func(cmd *cobra.Command, args []string) {
		id, err := uuid.Parse(flagRmID)
		if err != nil {
			log.Fatalf("Error parsing task ID: %v", err)
		}
		ctx := context.Background()
		var delErr error
		for i := 0; i < flagRmRetries; i++ {
			log.Printf("Attempt %d/%d to remove %v", i+1, flagRmRetries, id)
			tasks, err := eq.Tasks(ctx, "", entroq.WithTaskID(id))
			if err != nil {
				log.Fatalf("Error getting task ID %v", id)
			}
			if len(tasks) < 1 {
				log.Fatalf("Could not find task ID %v", id)
			}
			if len(tasks) > 1 {
				log.Fatalf("Too many tasks returned: %v", tasks)
			}

			task := tasks[0]

			modArgs := []entroq.ModifyArg{task.AsDeletion()}
			if flagRmForce {
				modArgs = append(modArgs, entroq.ModifyAs(task.Claimant))
			}

			_, mod, err := eq.Modify(ctx, modArgs...)
			if err != nil {
				log.Printf("Try %d/%d - could not remove task %v: %v", i+1, flagRmRetries, id, err)
				delErr = err
				time.Sleep(3 * time.Second)
				continue
			}

			b, err := json.MarshalIndent(mod, "", "\t")
			if err != nil {
				log.Fatalf("JSON marshal: %v", err)
			}

			fmt.Println(string(b))
			return
		}
		log.Fatalf("Could not delete task %v: %v", id, delErr)
	},
}
