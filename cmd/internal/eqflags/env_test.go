package eqflags

import (
	"flag"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func TestApplySetFromEnvironment(t *testing.T) {
	t.Setenv("EQTEST_READINESS_INTERVAL", "2s")
	t.Setenv("EQTEST_PORT", "37707")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	var interval time.Duration
	var port int
	flags.DurationVar(&interval, "readiness-interval", 5*time.Second, "")
	flags.IntVar(&port, "port", 37706, "")

	if err := ApplySet(NewEnvironment("EQTEST"), flags); err != nil {
		t.Fatal(err)
	}
	if interval != 2*time.Second {
		t.Errorf("readiness interval = %v, want 2s", interval)
	}
	if port != 37707 {
		t.Errorf("port = %d, want 37707", port)
	}
}

func TestApplySetCommandLineWins(t *testing.T) {
	t.Setenv("EQTEST_READINESS_INTERVAL", "2s")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	var interval time.Duration
	flags.DurationVar(&interval, "readiness-interval", 5*time.Second, "")
	if err := flags.Parse([]string{"--readiness-interval=3s"}); err != nil {
		t.Fatal(err)
	}

	if err := ApplySet(NewEnvironment("EQTEST"), flags); err != nil {
		t.Fatal(err)
	}
	if interval != 3*time.Second {
		t.Errorf("readiness interval = %v, want command-line value 3s", interval)
	}
}

func TestApplySetRejectsInvalidValue(t *testing.T) {
	t.Setenv("EQTEST_READINESS_INTERVAL", "eventually")

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	var interval time.Duration
	flags.DurationVar(&interval, "readiness-interval", 5*time.Second, "")

	err := ApplySet(NewEnvironment("EQTEST"), flags)
	if err == nil {
		t.Fatal("invalid duration was accepted")
	}
	if !strings.Contains(err.Error(), "--readiness-interval") {
		t.Fatalf("error %q does not identify --readiness-interval", err)
	}
}

func TestApplyIncludesLocalAndPersistentCobraFlags(t *testing.T) {
	t.Setenv("EQTEST_PORT", "37707")
	t.Setenv("EQTEST_READINESS_INTERVAL", "2s")

	var port int
	var interval time.Duration
	root := &cobra.Command{Use: "root", PersistentPreRunE: Apply(NewEnvironment("EQTEST"))}
	root.PersistentFlags().IntVar(&port, "port", 37706, "")
	serve := &cobra.Command{
		Use: "serve",
		RunE: func(*cobra.Command, []string) error {
			if port != 37707 || interval != 2*time.Second {
				t.Fatalf("values at RunE: port=%d interval=%v", port, interval)
			}
			return nil
		},
	}
	serve.Flags().DurationVar(&interval, "readiness_interval", 5*time.Second, "")
	root.AddCommand(serve)
	root.SetArgs([]string{"serve"})

	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyGoSetFromEnvironmentAndCommandLineWins(t *testing.T) {
	t.Setenv("EQTEST_RESYNC_INTERVAL", "2m")
	t.Setenv("EQTEST_METRICS_BIND_ADDRESS", ":9090")

	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	var interval time.Duration
	var address string
	flags.DurationVar(&interval, "resync-interval", 5*time.Minute, "")
	flags.StringVar(&address, "metrics-bind-address", "0", "")
	if err := flags.Parse([]string{"--resync-interval=3m"}); err != nil {
		t.Fatal(err)
	}

	if err := ApplyGoSet(NewEnvironment("EQTEST"), flags); err != nil {
		t.Fatal(err)
	}
	if interval != 3*time.Minute {
		t.Errorf("resync interval = %v, want command-line value 3m", interval)
	}
	if address != ":9090" {
		t.Errorf("metrics address = %q, want :9090", address)
	}
}
