package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var flagTryClaimID = struct {
	queue    string
	id       string
	duration int
}{}

func init() {
	flags := tryClaimIDCmd.Flags()
	flags.StringVarP(&flagTryClaimID.queue, "queue", "q", "", "Queue containing the task (required).")
	flags.StringVarP(&flagTryClaimID.id, "task", "t", "", "Task ID to claim (required).")
	flags.IntVarP(&flagTryClaimID.duration, "duration", "d", 30, "Claim duration in seconds.")

	tryClaimIDCmd.MarkFlagRequired("queue")
	tryClaimIDCmd.MarkFlagRequired("task")

	rootCmd.AddCommand(tryClaimIDCmd)
}

var tryClaimIDCmd = &cobra.Command{
	Use:   "tryclaimid",
	Short: "Optimistically claim a specific task by ID.",
	Long: `Attempts to claim a task identified by --task in --queue.

The task is claimable if its claim has expired or it has never been claimed.
On success the task is printed as JSON with the caller's claimant ID set.

If the task is still held by another claimant, exits with a non-zero status
and prints a message. If another process modified the task between the read
and the claim attempt (lost the optimistic race), the error is reported and
the caller may retry.

Set EQC_CLAIMANT (or --claimant) so that subsequent eqc commands in the same
session use the same claimant ID and can modify or delete the claimed task.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		tasks, err := eq.Tasks(ctx, flagTryClaimID.queue, entroq.WithTaskID(flagTryClaimID.id))
		if err != nil {
			return fmt.Errorf("get task: %w", err)
		}
		if len(tasks) == 0 {
			return fmt.Errorf("task %s not found in queue %s", flagTryClaimID.id, flagTryClaimID.queue)
		}
		task := tasks[0]

		duration := time.Duration(flagTryClaimID.duration) * time.Second
		resp, err := eq.Modify(ctx, task.Change(entroq.ArrivalTimeBy(duration)))
		if err != nil {
			var de *entroq.DependencyError
			if errors.As(err, &de) {
				if de.HasClaims() {
					return fmt.Errorf("task %s is still claimed by another claimant", flagTryClaimID.id)
				}
				// Version changed between Tasks and Modify -- lost the race.
				return fmt.Errorf("task %s was modified concurrently, retry: %w", flagTryClaimID.id, err)
			}
			return fmt.Errorf("claim task: %w", err)
		}

		fmt.Println(mustTaskString(resp.ChangedTasks[0]))
		return nil
	},
}
