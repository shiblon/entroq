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

	"entrogo.com/entroq"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	flagModID      string
	flagModQueueTo string
	flagModVal     string
	flagModForce   bool
)

func init() {
	rootCmd.AddCommand(modCmd)

	modCmd.Flags().StringVarP(&flagModID, "task", "t", "", "Task ID to modify. Note that this will modify *any* version of this task ID without regard for what else is happening. Use with care.")
	modCmd.MarkFlagRequired("task")
	modCmd.Flags().StringVarP(&flagModQueueTo, "queue_to", "Q", "", "New queue for task, if a change is desired.")
	modCmd.Flags().StringVarP(&flagModVal, "val", "v", "", "Value to set in task.")
	modCmd.Flags().BoolVarP(&flagModForce, "force", "f", false, "Force by spoofing the claimant if already claimed.")
}

// modCmd represents the mod command
var modCmd = &cobra.Command{
	Use:   "mod",
	Short: "Modify a task by queue and ID.",
	Run: func(cmd *cobra.Command, args []string) {
		id, err := uuid.Parse(flagModID)
		if err != nil {
			log.Fatalf("Error parsing task ID: %v", err)
		}
		tasks, err := eq.Tasks(context.Background(), "", entroq.WithTaskID(id))
		if err != nil {
			log.Fatalf("Error getting task ID %q", id)
		}
		if len(tasks) < 1 {
			log.Fatalf("Could not find task ID %q", id)
		}
		if len(tasks) > 1 {
			log.Fatalf("Too many tasks returned: %v", tasks)
		}

		task := tasks[0]

		var chgArgs []entroq.ChangeArg
		if flagModVal != "" {
			chgArgs = append(chgArgs, entroq.ValueTo([]byte(flagModVal)))
		}
		if flagModQueueTo != "" {
			chgArgs = append(chgArgs, entroq.QueueTo(flagModQueueTo))
		}

		modArgs := []entroq.ModifyArg{task.AsChange(chgArgs...)}
		if flagModForce {
			modArgs = append(modArgs, entroq.ModifyAs(task.Claimant))
		}
		if _, _, err := eq.Modify(context.Background(), modArgs...); err != nil {
			log.Fatalf("Could not modify task %q: %v", id, err)
		}
	},
}
