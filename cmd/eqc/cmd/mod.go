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

var flagMod = struct {
	id           string
	queueTo      string
	resetToQueue string
	val          string
	force        bool
	reset        bool
}{}

func init() {
	rootCmd.AddCommand(modCmd)

	modCmd.Flags().StringVarP(&flagMod.id, "task", "t", "", "Task ID to modify. Note that this will modify *any* version of this task ID without regard for what else is happening. Use with care.")
	modCmd.MarkFlagRequired("task")
	modCmd.Flags().StringVarP(&flagMod.queueTo, "queue_to", "Q", "", "New queue for task, if a change is desired. Keeps the task's ID, claim count, attempt, and error.")
	modCmd.Flags().StringVarP(&flagMod.resetToQueue, "reset_to_queue", "R", "", "Put the task back into service in this queue as a fresh task, e.g., out of an error queue. Deletes the task and inserts its value (or --val) with a new ID, zero claims and attempts, no error, and an arrival time of now, in one atomic modification. Prints the new task. Does not work on wrapped tasks (will just move the wrapping task, not the internals).")
	modCmd.MarkFlagsMutuallyExclusive("queue_to", "reset_to_queue")
	modCmd.Flags().StringVarP(&flagMod.val, "val", "v", "", "Value to set in task.")
	modCmd.Flags().BoolVarP(&flagMod.force, "force", "f", false, "Force by spoofing the claimant if already claimed.")

	// The old --reset reset attempts and errors but kept the claim count, so a
	// worker with a claim ceiling quarantined the task again on its first claim.
	// Kept only to point at its replacement.
	modCmd.Flags().BoolVarP(&flagMod.reset, "reset", "r", false, "Removed: use --reset_to_queue.")
	modCmd.Flags().MarkHidden("reset")
}

// modCmd represents the mod command
var modCmd = &cobra.Command{
	Use:   "mod",
	Short: "Modify a task by ID.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if flagMod.reset {
			return fmt.Errorf("--reset has been removed: it kept the claim count, so a worker's claim ceiling could quarantine the task again; use --reset_to_queue <queue>")
		}

		tasks, err := eq.Tasks(context.Background(), "", entroq.WithTaskID(flagMod.id))
		if err != nil {
			return fmt.Errorf("get task %q: %w", flagMod.id, err)
		}
		if len(tasks) < 1 {
			return fmt.Errorf("task not found: %q", flagMod.id)
		}
		if len(tasks) > 1 {
			return fmt.Errorf("too many tasks returned: %v", tasks)
		}

		task := tasks[0]

		var raw json.RawMessage
		if flagMod.val != "" {
			if raw, err = cliJSON(flagMod.val); err != nil {
				return err
			}
		}

		var modArgs []entroq.ModifyArg
		if flagMod.resetToQueue != "" {
			// Claims is backend-owned and survives every change, so only a
			// fresh task starts it over. Deleting at the version just read
			// keeps a concurrent change from leaving a duplicate behind.
			value := task.Value
			if raw != nil {
				value = raw
			}
			modArgs = append(modArgs,
				task.Delete(),
				entroq.InsertingInto(flagMod.resetToQueue, entroq.WithRawValue(value)),
			)
		} else {
			var chgArgs []entroq.ChangeArg
			if raw != nil {
				chgArgs = append(chgArgs, entroq.RawValueTo(raw))
			}
			if flagMod.queueTo != "" {
				chgArgs = append(chgArgs, entroq.QueueTo(flagMod.queueTo))
			}
			modArgs = append(modArgs, task.Change(chgArgs...))
		}
		if flagMod.force {
			modArgs = append(modArgs, entroq.ModifyAs(task.Claimant))
		}

		resp, err := eq.Modify(context.Background(), modArgs...)
		if err != nil {
			return fmt.Errorf("modify task %q: %w", flagMod.id, err)
		}
		if flagMod.resetToQueue != "" {
			fmt.Println(mustTaskString(resp.InsertedTasks[0]))
		}
		return nil
	},
}
