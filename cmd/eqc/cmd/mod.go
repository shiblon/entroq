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

var flagMod = struct {
	id      string
	queueTo string
	val     string
	force   bool
	reset   bool
}{}

func init() {
	rootCmd.AddCommand(modCmd)

	modCmd.Flags().StringVarP(&flagMod.id, "task", "t", "", "Task ID to modify. Note that this will modify *any* version of this task ID without regard for what else is happening. Use with care.")
	modCmd.MarkFlagRequired("task")
	modCmd.Flags().StringVarP(&flagMod.queueTo, "queue_to", "Q", "", "New queue for task, if a change is desired.")
	modCmd.Flags().BoolVarP(&flagMod.reset, "reset", "r", false, "Reset Attempt and Err while changing the task, e.g., moving from an error queue back into service. Does not work on wrapped tasks (will just move the wrapping task, not the internals).")
	modCmd.Flags().StringVarP(&flagMod.val, "val", "v", "", "Value to set in task.")
	modCmd.Flags().BoolVarP(&flagMod.force, "force", "f", false, "Force by spoofing the claimant if already claimed.")
}

// modCmd represents the mod command
var modCmd = &cobra.Command{
	Use:   "mod",
	Short: "Modify a task by queue and ID.",
	Run: func(cmd *cobra.Command, args []string) {
		id, err := uuid.Parse(flagMod.id)
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
		if flagMod.val != "" {
			chgArgs = append(chgArgs, entroq.ValueTo([]byte(flagMod.val)))
		}
		if flagMod.queueTo != "" {
			chgArgs = append(chgArgs, entroq.QueueTo(flagMod.queueTo))
		}

		modArgs := []entroq.ModifyArg{task.AsChange(chgArgs...)}
		if flagMod.force {
			modArgs = append(modArgs, entroq.ModifyAs(task.Claimant))
		}

		if flagMod.reset {
			modArgs = append(modArgs, entroq.AttemptToZero(), entroq.ErrToZero())
		}
		if _, _, err := eq.Modify(context.Background(), modArgs...); err != nil {
			log.Fatalf("Could not modify task %q: %v", id, err)
		}
	},
}
