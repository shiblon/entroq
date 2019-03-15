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

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	flagTsQueue string
)

func init() {
	rootCmd.AddCommand(tsCmd)

	tsCmd.Flags().StringVarP(&flagTsQueue, "queue", "q", "", "Queue to read tasks from. Required.")
	tsCmd.MarkFlagRequired("queue")
}

// tsCmd represents the ts command
var tsCmd = &cobra.Command{
	Use:   "ts",
	Short: "Get tasks from a queue in the EntroQ",
	RunE: func(cmd *cobra.Command, args []string) error {
		ts, err := eq.Tasks(context.Background(), flagTsQueue)
		if err != nil {
			return errors.Wrap(err, "get tasks")
		}

		b, err := json.MarshalIndent(ts, "", "\t")
		if err != nil {
			return errors.Wrap(err, "tasks json")
		}

		fmt.Println(string(b))
		return nil
	},
}
