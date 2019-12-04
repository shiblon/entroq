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
	"fmt"
	"log"

	"entrogo.com/entroq"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	flagTsQueue string
	flagTsIDs   []string
	flagTsLimit int
)

func init() {
	rootCmd.AddCommand(tsCmd)

	tsCmd.Flags().StringVarP(&flagTsQueue, "queue", "q", "", "Queue to read tasks from. Can be blank if IDs are given.")
	tsCmd.Flags().StringArrayVarP(&flagTsIDs, "task", "t", nil, "Task IDs to read from. Optional.")
	tsCmd.Flags().IntVarP(&flagTsLimit, "limit", "n", 0, "Limit of tasks, unlimited if 0.")
}

// tsCmd represents the ts command
var tsCmd = &cobra.Command{
	Use:   "ts",
	Short: "Get tasks from a queue in the EntroQ",
	Run: func(cmd *cobra.Command, args []string) {
		if len(flagTsIDs) == 0 && flagTsQueue == "" {
			log.Print("No queue or task IDs specified.")
			return
		}
		var ids []uuid.UUID
		for _, tid := range flagTsIDs {
			uid, err := uuid.Parse(tid)
			if err != nil {
				log.Fatalf("Failed to parse ID %q: %v", tid, err)
			}
			ids = append(ids, uid)
		}
		fmt.Println(flagTsLimit)
		ts, err := eq.Tasks(context.Background(), flagTsQueue, entroq.WithTaskID(ids...), entroq.LimitTasks(flagTsLimit))
		if err != nil {
			log.Fatalf("Error getting tasks: %v", err)
		}
		for _, t := range ts {
			fmt.Println(mustTaskString(t))
		}
	},
}
