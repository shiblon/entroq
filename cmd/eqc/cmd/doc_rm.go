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

	"github.com/shiblon/entroq"
	"github.com/spf13/cobra"
)

var (
	flagDocRmNamespace string
	flagDocRmID        string
)

func init() {
	rootCmd.AddCommand(docRmCmd)

	docRmCmd.Flags().StringVarP(&flagDocRmNamespace, "namespace", "n", "", "Namespace of the doc. Required.")
	docRmCmd.MarkFlagRequired("namespace")

	docRmCmd.Flags().StringVarP(&flagDocRmID, "id", "i", "", "ID of the doc to delete. Required.")
	docRmCmd.MarkFlagRequired("id")
}

var docRmCmd = &cobra.Command{
	Use:   "doc-rm",
	Short: "Delete a doc by namespace and ID.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		docs, err := eq.Docs(ctx, &entroq.DocQuery{
			Namespace: flagDocRmNamespace,
			IDs:       []string{flagDocRmID},
		})
		if err != nil {
			return fmt.Errorf("doc-rm lookup: %w", err)
		}
		if len(docs) == 0 {
			log.Fatalf("doc not found: %s/%s", flagDocRmNamespace, flagDocRmID)
		}

		doc := docs[0]
		if _, err := eq.Modify(ctx, doc.Delete()); err != nil {
			return fmt.Errorf("doc-rm: %w", err)
		}
		fmt.Printf("deleted doc %s/%s@%d\n", doc.Namespace, doc.ID, doc.Version)
		return nil
	},
}
