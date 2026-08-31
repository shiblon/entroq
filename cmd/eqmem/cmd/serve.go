package cmd

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/shiblon/entroq"
	"github.com/shiblon/entroq/cmd/internal/eqserve"
	"github.com/shiblon/entroq/pkg/backend/eqmem"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel/metric"
)

const minSnapshotPeriod = time.Minute

var serve struct {
	eqserve.Config

	journal          string
	createJournalDir bool
	snapshotAndQuit  bool
	periodicSnapshot string
	journalMaxItems  int
	journalMaxBytes  int
	cleanup          bool
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the EntroQ gRPC and HTTP/JSON service.",
	Long: `Serve an in-memory EntroQ backend over gRPC (--port, default 37706) and an
HTTP/JSON + Connect API (--http_port, default 9100, which also serves /metrics).

State is held in memory. Pass --journal to persist it to a write-ahead journal
that replays quickly on restart; without one, a restart starts empty. Best for
tests, development, and light-duty singleton services.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		if serve.journal == "" {
			if serve.snapshotAndQuit || serve.cleanup || serve.periodicSnapshot != "" || serve.createJournalDir {
				return fmt.Errorf("journal settings given but no --journal directory specified")
			}
		}

		if serve.cleanup && !serve.snapshotAndQuit && serve.periodicSnapshot == "" {
			return fmt.Errorf("--journal_cleanup requires --snapshot_and_quit or --periodic_snapshot")
		}

		if serve.periodicSnapshot != "" && serve.snapshotAndQuit {
			return fmt.Errorf("--periodic_snapshot and --snapshot_and_quit are mutually exclusive")
		}

		if serve.createJournalDir {
			if err := os.MkdirAll(serve.journal, 0700); err != nil {
				return fmt.Errorf("create journal dir: %w", err)
			}
		}

		if serve.snapshotAndQuit {
			if err := eqmem.TakeSnapshot(ctx, serve.journal, serve.cleanup); err != nil {
				return fmt.Errorf("take snapshot in %q: %w", serve.journal, err)
			}
			return nil
		}

		if psf := serve.periodicSnapshot; psf != "" {
			period, err := time.ParseDuration(psf)
			if err != nil {
				return fmt.Errorf("periodic snapshot %q: not a valid duration: %w", psf, err)
			}
			if period < minSnapshotPeriod {
				log.Printf("Snapshot period %v smaller than %v: clamping", period, minSnapshotPeriod)
				period = minSnapshotPeriod
			}
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-time.After(period):
						if err := eqmem.TakeSnapshot(ctx, serve.journal, serve.cleanup); err != nil {
							log.Printf("Periodic snapshot %q: %v", serve.journal, err)
						}
					}
				}
			}()
		}

		return eqserve.Run(ctx, serve.Config,
			func(mp metric.MeterProvider) entroq.BackendOpener {
				return eqmem.Opener(
					eqmem.WithJournal(serve.journal),
					eqmem.WithMaxJournalBytes(int64(serve.journalMaxBytes)),
					eqmem.WithMaxJournalItems(serve.journalMaxItems),
					eqmem.WithMeterProvider(mp),
				)
			},
			fmt.Sprintf("eqmem(journal=%q)", serve.journal),
		)
	},
}

func init() {
	f := serveCmd.Flags()
	serve.Config.BindFlags(f)
	f.StringVar(&serve.journal, "journal", "", "Journal directory for persistence. Default is ephemeral.")
	f.BoolVar(&serve.createJournalDir, "mkdir", false, "Create the journal directory if it does not exist.")
	f.BoolVar(&serve.snapshotAndQuit, "snapshot_and_quit", false, "Read the journal, write a snapshot, then exit. Requires --journal.")
	f.StringVar(&serve.periodicSnapshot, "periodic_snapshot", "", "Snapshot interval (e.g. 1h). Minimum 1m. Requires --journal.")
	f.BoolVar(&serve.cleanup, "journal_cleanup", false, "Remove compacted journal files after snapshotting. Requires --journal.")
	f.IntVar(&serve.journalMaxItems, "journal_max_items", 0, "Rotate journal after this many items (0 uses the built-in default).")
	f.IntVar(&serve.journalMaxBytes, "journal_max_bytes", 0, "Rotate journal after this many bytes (0 uses the built-in default).")

	rootCmd.AddCommand(serveCmd)
}
