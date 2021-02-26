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

	"entrogo.com/entroq"
	"github.com/spf13/cobra"
)

var (
	flagInsQueue string
	flagInsVal   []string
)

func init() {
	rootCmd.AddCommand(insCmd)

	insCmd.Flags().StringVarP(&flagInsQueue, "queue", "q", "", "Queue to insert task into. Required.")
	insCmd.MarkFlagRequired("queue")

	insCmd.Flags().StringArrayVarP(&flagInsVal, "val", "v", nil, "Value to insert, can be specified more than once to insert more than one task. Specify an empty value to insert a task with no value.")
}

// insCmd represents the ins command
var insCmd = &cobra.Command{
	Use:   "ins",
	Short: "Insert a task into EntroQ.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(flagInsVal) == 0 {
			flagInsVal = append(flagInsVal, "")
		}
		var insArgs []entroq.InsertArg
		for _, v := range flagInsVal {
			insArgs = append(insArgs, entroq.WithValue([]byte(v)))
		}

		ins, _, err := eq.Modify(context.Background(), entroq.InsertingInto(flagInsQueue, insArgs...))
		if err != nil {
			return fmt.Errorf("insert tasks: %w", err)
		}

		b, err := json.MarshalIndent(ins, "", "\t")
		if err != nil {
			return fmt.Errorf("insert json: %w", err)
		}
		fmt.Println(string(b))

		return nil
	},
}
