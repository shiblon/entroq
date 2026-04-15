package cmd

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	authz "github.com/shiblon/entroq/pkg/authz/opadata"
	"github.com/spf13/cobra"
)

// seedData is the template data.json written to local/ on opa init.
// The admin fills in their IDP values and policy.
var seedData = map[string]any{
	"entroq": map[string]any{
		"idp": map[string]any{
			"jwks_url": "https://your-idp/.well-known/jwks.json",
			"audience": "your-audience",
			"issuer":   "https://your-idp/",
		},
		"policy": map[string]any{
			"users": []any{
				map[string]any{
					"name":   "user-subject-from-jwt",
					"roles":  []any{"editor"},
					"queues": []any{},
				},
			},
			"roles": []any{
				map[string]any{
					"name": "editor",
					"queues": []any{
						map[string]any{
							"prefix":  "/shared/",
							"actions": []any{"CLAIM", "INSERT", "DELETE"},
						},
					},
				},
			},
		},
	},
}

var opaCmd = &cobra.Command{
	Use:   "opa",
	Short: "Manage OPA policy configuration for EntroQ authorization.",
}

var flagOpaInitForce bool

var opaInitCmd = &cobra.Command{
	Use:   "init <dir>",
	Short: "Seed a local OPA configuration directory.",
	Long: `Creates a local OPA configuration directory ready for use with EntroQ.

The directory will contain:

  <dir>/core/       - core EntroQ Rego logic (do not modify)
  <dir>/providers/  - built-in OIDC user and permissions provider (do not modify)
  <dir>/local/
    data.json       - edit this: IDP settings, users, and roles

Mount these into the OPA container:
  core/      -> /etc/opa/core
  providers/ -> /etc/opa/providers
  local/     -> /etc/opa/local

See pkg/authz/opadata/OPA_AUTHZ.md for full configuration reference.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		dir := args[0]
		if err := opaInit(dir, flagOpaInitForce); err != nil {
			log.Fatalf("opa init: %v", err)
		}
		fmt.Printf("OPA config initialized in %s\n", dir)
		fmt.Printf("Edit %s/local/data.json to configure your IDP and policy.\n", dir)
	},
}

var opaUpgradeCmd = &cobra.Command{
	Use:   "upgrade <dir>",
	Short: "Upgrade core/ and providers/ in an existing OPA config directory.",
	Long: `Refreshes the core/ and providers/ directories from the current
EntroQ release. Your local/data.json is not modified.

Run this after upgrading EntroQ to pick up any policy changes.`,
	Args: cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		dir := args[0]
		if err := opaUpgrade(dir); err != nil {
			log.Fatalf("opa upgrade: %v", err)
		}
		fmt.Printf("OPA core and providers upgraded in %s\n", dir)
	},
}

func init() {
	opaInitCmd.Flags().BoolVarP(&flagOpaInitForce, "force", "f", false, "Overwrite existing core/ and providers/ directories.")
	opaCmd.AddCommand(opaInitCmd)
	opaCmd.AddCommand(opaUpgradeCmd)
	rootCmd.AddCommand(opaCmd)
}

// opaInit seeds a new OPA config directory.
func opaInit(dir string, force bool) error {
	localDir := filepath.Join(dir, "local")
	dataFile := filepath.Join(localDir, "data.json")

	if err := extractConfFS(dir, force); err != nil {
		return err
	}

	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("create local dir: %w", err)
	}

	if _, err := os.Stat(dataFile); err == nil {
		fmt.Printf("  skipping %s (already exists)\n", dataFile)
		return nil
	}

	b, err := json.MarshalIndent(seedData, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal seed data: %w", err)
	}
	if err := os.WriteFile(dataFile, append(b, '\n'), 0644); err != nil {
		return fmt.Errorf("write data.json: %w", err)
	}
	fmt.Printf("  wrote %s\n", dataFile)
	return nil
}

// opaUpgrade refreshes core/ and providers/ from the embedded FS.
func opaUpgrade(dir string) error {
	return extractConfFS(dir, true)
}

// extractConfFS copies core/ and providers/ from the embedded FS into dir,
// stripping the leading "conf/" prefix so they land at <dir>/core/ and
// <dir>/providers/ rather than <dir>/conf/core/.
func extractConfFS(dir string, overwrite bool) error {
	return fs.WalkDir(authz.ConfFS, "conf", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Strip the leading "conf/" prefix.
		rel := strings.TrimPrefix(path, "conf/")
		if rel == "conf" || rel == "" {
			return nil
		}

		dest := filepath.Join(dir, rel)

		if d.IsDir() {
			return os.MkdirAll(dest, 0755)
		}

		if !overwrite {
			if _, err := os.Stat(dest); err == nil {
				fmt.Printf("  skipping %s (already exists)\n", dest)
				return nil
			}
		}

		data, err := authz.ConfFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		fmt.Printf("  wrote %s\n", dest)
		return nil
	})
}
