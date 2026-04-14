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

var (
	flagDocPutNamespace    string
	flagDocPutKey          string
	flagDocPutSecondaryKey string
	flagDocPutContent      string
)

func init() {
	rootCmd.AddCommand(docPutCmd)

	docPutCmd.Flags().StringVarP(&flagDocPutNamespace, "namespace", "n", "", "Namespace for the doc. Required.")
	docPutCmd.MarkFlagRequired("namespace")

	docPutCmd.Flags().StringVarP(&flagDocPutKey, "key", "k", "", "Primary key for the doc. Required.")
	docPutCmd.MarkFlagRequired("key")

	docPutCmd.Flags().StringVarP(&flagDocPutSecondaryKey, "secondary", "K", "", "Secondary key for the doc. Optional.")
	docPutCmd.Flags().StringVarP(&flagDocPutContent, "content", "v", "", "JSON content for the doc. Empty becomes null.")
}

var docPutCmd = &cobra.Command{
	Use:   "doc-put",
	Short: "Create a new doc in the EntroQ doc store.",
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, err := cliJSON(flagDocPutContent)
		if err != nil {
			return fmt.Errorf("doc content: %w", err)
		}

		resp, err := eq.Modify(context.Background(),
			entroq.CreatingIn(flagDocPutNamespace,
				entroq.WithKeys(flagDocPutKey, flagDocPutSecondaryKey),
				entroq.WithRawContent(raw),
			),
		)
		if err != nil {
			return fmt.Errorf("doc-put: %w", err)
		}

		b, err := json.Marshal(resp.InsertedDocs)
		if err != nil {
			return fmt.Errorf("doc-put json: %w", err)
		}
		fmt.Println(string(b))
		return nil
	},
}
