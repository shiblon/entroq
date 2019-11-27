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
	"time"

	"github.com/spf13/cobra"
)

var (
	flagTimeMillis bool
	flagTimeLocal  bool
)

func init() {
	rootCmd.AddCommand(timeCmd)

	timeCmd.Flags().BoolVarP(&flagTimeMillis, "millis", "m", false, "Return time as milliseconds since the Epoch, UTC")

	timeCmd.Flags().BoolVarP(&flagTimeLocal, "local", "l", false, "Show local time")
}

// timeCmd gets the time from the backend.
var timeCmd = &cobra.Command{
	Use:   "time",
	Short: "Get time from the backend.",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()

		t, err := eq.Time(ctx)
		if err != nil {
			return err
		}
		if flagTimeMillis {
			fmt.Println(t.Truncate(time.Millisecond).UnixNano() / 1000000)
			return nil
		}

		if flagTimeLocal {
			t = t.Local()
		}

		fmt.Println(t)
		return nil
	},
}
