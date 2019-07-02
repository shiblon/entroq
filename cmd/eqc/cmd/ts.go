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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"

	"github.com/spf13/cobra"
)

var (
	flagTsQueue string
	flagTsJSON  bool
)

func init() {
	rootCmd.AddCommand(tsCmd)

	tsCmd.Flags().StringVarP(&flagTsQueue, "queue", "q", "", "Queue to read tasks from. Required.")
	tsCmd.MarkFlagRequired("queue")
	tsCmd.Flags().BoolVarP(&flagTsJSON, "json", "j", false, "Value is JSON, display as such.")
}

// tsCmd represents the ts command
var tsCmd = &cobra.Command{
	Use:   "ts",
	Short: "Get tasks from a queue in the EntroQ",
	Run: func(cmd *cobra.Command, args []string) {
		ts, err := eq.Tasks(context.Background(), flagTsQueue)
		if err != nil {
			log.Fatalf("Error getting tasks: %v", err)
		}

		b, err := json.MarshalIndent(ts, "", "\t")
		if err != nil {
			log.Fatalf("Error getting task JSON: %v", err)
		}

		if flagTsJSON {
			var tasks []map[string]interface{}
			if err := json.Unmarshal(b, &tasks); err != nil {
				log.Fatalf("Error converting JSON into maps: %v", err)
			}
			for _, t := range tasks {
				var m map[string]interface{}
				v, err := base64.StdEncoding.DecodeString(t["value"].(string))
				if err != nil {
					log.Fatalf("Error decoding json base64 byte string: %v", err)
				}
				if err := json.Unmarshal([]byte(v), &m); err != nil {
					log.Fatalf("Error getting map from value JSON: %v", err)
				}
				t["value"] = m
			}
			b, err = json.MarshalIndent(tasks, "", "\t")
			if err != nil {
				log.Fatalf("Error getting task JSON: %v", err)
			}
		}

		fmt.Println(string(b))
	},
}
