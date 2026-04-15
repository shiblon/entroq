package cmd

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/pkg/backend/eqgrpc"
	"github.com/shiblon/entroq/pkg/version"
	"github.com/shiblon/entroq/pkg/workers/appendworker"
	"github.com/shiblon/stuffedio/wal"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	cfgFile    string
	eqaddr     string
	inbox      string
	journalDir string
	maxBytes   int64
	maxIndices int
)

var rootCmd = &cobra.Command{
	Use:     "eqjournal [options]",
	Version: version.Version,
	Short:   "EntroQ Worker CLI that appends tasks to a stuffedio journal",
	Long: `The eqjournal processes EntroQ tasks by appending their values to a 
rotating stuffedio journal. This is useful for building durable audit trails 
and agent reasoning logs.`,
	Run: func(cmd *cobra.Command, args []string) {
		if inbox == "" {
			log.Fatal("No inbox specified.")
		}
		if journalDir == "" {
			log.Fatal("No journal directory specified.")
		}

		ctx := context.Background()
		eq, err := entroq.New(ctx, eqgrpc.Opener(eqaddr, eqgrpc.WithInsecure()))
		if err != nil {
			log.Fatalf("Error opening EntroQ service: %v", err)
		}
		defer eq.Close()

		// Open the WAL.
		journal, err := wal.Open(ctx, journalDir,
			wal.WithMaxJournalBytes(maxBytes),
			wal.WithMaxJournalIndices(maxIndices),
			wal.WithAllowWrite(true),
		)
		if err != nil {
			log.Fatalf("Error opening journal WAL: %v", err)
		}
		defer journal.Close()

		log.Printf("Starting journal worker for %q on inbox %q, writing to %q", eqaddr, inbox, journalDir)
		if err := appendworker.Run(ctx, eq, journal, appendworker.Watching(inbox)); err != nil {
			log.Fatalf("Error executing worker: %v", err)
		}
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("Error executing root command: %v", err)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	pflags := rootCmd.PersistentFlags()
	pflags.StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/eqjournal.yaml)")

	pflags.StringVar(&inbox, "inbox", "/agentq/journal", "Queue to listen to")
	pflags.StringVar(&eqaddr, "eqaddr", ":37706", "address of service, uses port 37706 if none is specified")
	pflags.StringVar(&journalDir, "journal_dir", "./journals", "Directory for stuffedio journals")
	pflags.Int64Var(&maxBytes, "max_bytes", wal.DefaultMaxBytes, "Max bytes per journal file before rotation")
	pflags.IntVar(&maxIndices, "max_indices", wal.DefaultMaxIndices, "Max records per journal file before rotation")

	viper.BindPFlag("eqaddr", pflags.Lookup("eqaddr"))
}

func initConfig() {
	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		viper.AddConfigPath(filepath.Join(home, ".config"))
		viper.SetConfigName("eqjournal")
	}

	if !strings.Contains(eqaddr, ":") {
		eqaddr += ":37706"
	}

	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}
