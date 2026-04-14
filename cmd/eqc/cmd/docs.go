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

	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var (
	flagDocsNamespace  string
	flagDocsIDs        []string
	flagDocsKeyStart   string
	flagDocsKeyEnd     string
	flagDocsLimit      int
	flagDocsOmitValues bool
)

func init() {
	rootCmd.AddCommand(docsCmd)

	docsCmd.Flags().StringVarP(&flagDocsNamespace, "namespace", "n", "", "Namespace to list docs from. Required.")
	docsCmd.MarkFlagRequired("namespace")

	docsCmd.Flags().StringArrayVarP(&flagDocsIDs, "id", "i", nil, "Doc IDs to fetch. When given, key range and limit are ignored.")
	docsCmd.Flags().StringVarP(&flagDocsKeyStart, "key", "k", "", "Start of primary key range (inclusive).")
	docsCmd.Flags().StringVarP(&flagDocsKeyEnd, "key-end", "K", "", "End of primary key range (exclusive). Empty means unbounded.")
	docsCmd.Flags().IntVarP(&flagDocsLimit, "limit", "l", 0, "Maximum docs to return; 0 means no limit.")
	docsCmd.Flags().BoolVarP(&flagDocsOmitValues, "omit-values", "V", false, "Omit content in doc output.")
}

var docsCmd = &cobra.Command{
	Use:   "docs",
	Short: "List docs from a namespace in the EntroQ doc store.",
	RunE: func(cmd *cobra.Command, args []string) error {
		docs, err := eq.Docs(context.Background(), &entroq.DocQuery{
			Namespace:  flagDocsNamespace,
			IDs:        flagDocsIDs,
			KeyStart:   flagDocsKeyStart,
			KeyEnd:     flagDocsKeyEnd,
			Limit:      flagDocsLimit,
			OmitValues: flagDocsOmitValues,
		})
		if err != nil {
			return fmt.Errorf("list docs: %w", err)
		}
		for _, d := range docs {
			b, err := json.Marshal(d)
			if err != nil {
				log.Fatalf("JSON marshal doc: %v", err)
			}
			fmt.Println(string(b))
		}
		return nil
	},
}
