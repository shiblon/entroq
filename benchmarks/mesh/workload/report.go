package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func report(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	inputDir := flags.String("input-dir", "", "directory containing sample JSON files")
	backend := flags.String("backend", "memory", "EntroQ storage backend used by the deployment")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *inputDir == "" {
		return fmt.Errorf("input-dir is required")
	}
	switch *backend {
	case "memory", "redis", "postgres":
	default:
		return fmt.Errorf("backend must be memory, redis, or postgres")
	}
	paths, err := filepath.Glob(filepath.Join(*inputDir, "sample-*.json"))
	if err != nil {
		return fmt.Errorf("glob results: %w", err)
	}
	if len(paths) == 0 {
		return fmt.Errorf("no sample JSON files in %s", *inputDir)
	}

	results := make([]sampleResult, 0, len(paths))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		var result sampleResult
		decodeErr := json.NewDecoder(file).Decode(&result)
		closeErr := file.Close()
		if decodeErr != nil {
			return fmt.Errorf("decode %s: %w", path, decodeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", path, closeErr)
		}
		if result.SchemaVersion != resultSchemaVersion {
			return fmt.Errorf("%s has schema version %d, want %d", path, result.SchemaVersion, resultSchemaVersion)
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].Config.Sample == results[j].Config.Sample {
			return results[i].Config.Mode < results[j].Config.Mode
		}
		return results[i].Config.Sample < results[j].Config.Sample
	})

	first := results[0]
	for _, result := range results[1:] {
		if result.Config.AuthzStrategy != first.Config.AuthzStrategy || result.Config.AuthzProfile != first.Config.AuthzProfile {
			return fmt.Errorf("mixed authorization configurations %q/%q and %q/%q in one result directory",
				first.Config.AuthzStrategy, first.Config.AuthzProfile,
				result.Config.AuthzStrategy, result.Config.AuthzProfile)
		}
		if result.Config.TargetRPS != first.Config.TargetRPS {
			return fmt.Errorf("mixed target rates %.2f and %.2f in one result directory",
				first.Config.TargetRPS, result.Config.TargetRPS)
		}
	}
	fmt.Fprintln(out, "# EntroQ local-cluster mesh benchmark")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "This compares matched HTTP requests from an in-cluster load pod. The raw direct path is an unauthenticated transport lower bound; it is not a security-equivalent mesh baseline. The one- and two-hop paths use eqlink, an EntroQ %s backend, and the same leaf handler. All k3d nodes share one physical host, so these are local-cluster ", *backend)
	if first.Config.TargetRPS > 0 {
		fmt.Fprintln(out, "latency measurements at a fixed offered rate, not multi-machine production estimates.")
	} else {
		fmt.Fprintln(out, "capacity measurements, not multi-machine production estimates.")
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Configuration: %s backend, %d-byte payload, concurrency %d, EntroQ authorization `%s` with profile `%s`, %s measured after %s warm-up, per sample",
		*backend, first.Config.PayloadBytes, first.Config.Concurrency, first.Config.AuthzStrategy, first.Config.AuthzProfile, first.Config.DurationText, first.Config.WarmupText)
	if first.Config.TargetRPS > 0 {
		fmt.Fprintf(out, ", paced at %.2f total requests/second", first.Config.TargetRPS)
	}
	fmt.Fprintln(out, ".")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "```mermaid")
	fmt.Fprintln(out, "flowchart LR")
	fmt.Fprintln(out, "  L[load job] -->|raw direct HTTP| A[leaf handler]")
	if len(filterMode(results, "direct-auth")) > 0 {
		fmt.Fprintln(out, "  L -->|authorized direct HTTP| DA[OPA-gated proxy]")
		fmt.Fprintln(out, "  DA --> A")
	}
	fmt.Fprintln(out, "  L -->|HTTP| G[gateway sender]")
	queueLabel := "EntroQ " + *backend
	if first.Config.AuthzStrategy == "opahttp" {
		queueLabel += " + OPA"
	}
	fmt.Fprintf(out, "  G -->|one hop| Q[%s]\n", queueLabel)
	fmt.Fprintln(out, "  Q --> LR[leaf receiver]")
	fmt.Fprintln(out, "  LR --> A")
	fmt.Fprintln(out, "  G -->|two hops| Q")
	fmt.Fprintln(out, "  Q --> RR[relay receiver]")
	fmt.Fprintln(out, "  RR --> RS[relay sender]")
	fmt.Fprintln(out, "  RS --> Q")
	fmt.Fprintln(out, "```")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "| Mode | Samples | Median req/s (range) | Median p50 | Median p95 | Median p99 | Failures | Invalid |")
	fmt.Fprintln(out, "|---|---:|---:|---:|---:|---:|---:|---:|")
	for _, mode := range []string{"direct-raw", "direct-auth", "direct", "mesh", "mesh2"} {
		group := filterMode(results, mode)
		if len(group) == 0 {
			continue
		}
		throughputs := sampleValues(group, func(r sampleResult) float64 { return r.Throughput })
		p50 := sampleValues(group, func(r sampleResult) float64 { return r.Latency.P50 })
		p95 := sampleValues(group, func(r sampleResult) float64 { return r.Latency.P95 })
		p99 := sampleValues(group, func(r sampleResult) float64 { return r.Latency.P99 })
		var failures, invalid int64
		for _, result := range group {
			failures += result.Failures
			invalid += result.InvalidResponses
		}
		fmt.Fprintf(out, "| %s | %d | %.1f (%.1f–%.1f) | %.2f ms | %.2f ms | %.2f ms | %d | %d |\n",
			modeLabel(mode), len(group), median(throughputs), throughputs[0], throughputs[len(throughputs)-1],
			median(p50), median(p95), median(p99), failures, invalid)
	}
	if directAuth := filterMode(results, "direct-auth"); len(directAuth) > 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "## Authorization-normalized comparison")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "The authorized direct path makes one OPA decision using the gateway identity and leaf queue policy. Latency ratios are paired by sample, which makes them less sensitive to shared-host changes than cross-run absolute values.")
		fmt.Fprintln(out)
		if first.Config.TargetRPS > 0 {
			fmt.Fprintln(out, "Throughput ratios are omitted because every mode receives the same fixed offered rate; this run measures latency with queue headroom, not capacity.")
			fmt.Fprintln(out)
			fmt.Fprintln(out, "| Mode | Median OPA decisions/request | p50 multiple vs authorized direct |")
			fmt.Fprintln(out, "|---|---:|---:|")
		} else {
			fmt.Fprintln(out, "| Mode | Median OPA decisions/request | Throughput penalty vs authorized direct | p50 multiple vs authorized direct |")
			fmt.Fprintln(out, "|---|---:|---:|---:|")
		}
		for _, mode := range []string{"direct-auth", "mesh", "mesh2"} {
			group := filterMode(results, mode)
			if len(group) == 0 {
				continue
			}
			decisions := make([]float64, 0, len(group))
			for _, result := range group {
				if count, ok := opaDecisionCount(result); ok && result.Completed > 0 {
					decisions = append(decisions, count/float64(result.Completed))
				}
			}
			if len(decisions) == 0 {
				continue
			}
			sort.Float64s(decisions)
			latencyRatios := pairedRatios(group, directAuth,
				func(r sampleResult) float64 { return r.Latency.P50 },
				func(r sampleResult) float64 { return r.Latency.P50 })
			if first.Config.TargetRPS > 0 {
				fmt.Fprintf(out, "| %s | %.2f | %.2fx |\n",
					modeLabel(mode), median(decisions), median(latencyRatios))
			} else {
				throughputRatios := pairedRatios(directAuth, group,
					func(r sampleResult) float64 { return r.Throughput },
					func(r sampleResult) float64 { return r.Throughput })
				fmt.Fprintf(out, "| %s | %.2f | %.2fx | %.2fx |\n",
					modeLabel(mode), median(decisions), median(throughputRatios), median(latencyRatios))
			}
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "## Samples")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "| Sample | Mode | Completed | req/s | p50 | p95 | p99 | max |")
	fmt.Fprintln(out, "|---:|---|---:|---:|---:|---:|---:|---:|")
	for _, result := range results {
		fmt.Fprintf(out, "| %d | %s | %d | %.1f | %.2f ms | %.2f ms | %.2f ms | %.2f ms |\n",
			result.Config.Sample, modeLabel(result.Config.Mode), result.Completed, result.Throughput,
			result.Latency.P50, result.Latency.P95, result.Latency.P99, result.Latency.Max)
	}

	metricsOK := observedPositiveMetric(results, "mesh", "gateway", "sender_handled_total") &&
		observedPositiveMetric(results, "mesh", "leaf", "receiver_handled_total")
	mesh2 := filterMode(results, "mesh2")
	if len(mesh2) > 0 {
		metricsOK = metricsOK &&
			observedPositiveMetric(results, "mesh2", "gateway", "sender_handled_total") &&
			observedPositiveMetric(results, "mesh2", "relay", "receiver_handled_total") &&
			observedPositiveMetric(results, "mesh2", "relay", "sender_handled_total") &&
			observedPositiveMetric(results, "mesh2", "leaf", "receiver_handled_total")
	}
	fmt.Fprintln(out)
	if metricsOK {
		fmt.Fprintln(out, "Telemetry check: source-specific sender and receiver handled counters were positive for every measured mesh hop.")
	} else {
		fmt.Fprintln(out, "Telemetry check: **failed** — mesh snapshots did not contain every source-specific sender and receiver handled counter.")
		return fmt.Errorf("mesh telemetry missing a source-specific sender or receiver handled counter")
	}
	return nil
}

func pairedRatios(numerators, denominators []sampleResult, numerator, denominator func(sampleResult) float64) []float64 {
	bySample := make(map[int]sampleResult, len(denominators))
	for _, result := range denominators {
		bySample[result.Config.Sample] = result
	}
	var ratios []float64
	for _, result := range numerators {
		other, ok := bySample[result.Config.Sample]
		if !ok || denominator(other) == 0 {
			continue
		}
		ratios = append(ratios, numerator(result)/denominator(other))
	}
	sort.Float64s(ratios)
	return ratios
}

func opaDecisionCount(result sampleResult) (float64, bool) {
	var first, last *metricSnapshot
	for i := range result.MetricSnapshots {
		snapshot := &result.MetricSnapshots[i]
		if snapshot.Source != "opa" || snapshot.Error != "" {
			continue
		}
		if _, ok := opaDecisionCounter(snapshot.Body); !ok {
			continue
		}
		if first == nil {
			first = snapshot
		}
		last = snapshot
	}
	if first == nil || last == nil || first == last {
		return 0, false
	}
	firstValue, _ := opaDecisionCounter(first.Body)
	lastValue, _ := opaDecisionCounter(last.Body)
	return lastValue - firstValue, lastValue >= firstValue
}

func opaDecisionCounter(body string) (float64, bool) {
	const metric = "http_request_duration_seconds_count{"
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric) ||
			!strings.Contains(line, `handler="v1/data"`) ||
			!strings.Contains(line, `method="post"`) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return 0, false
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		return value, err == nil
	}
	return 0, false
}

func filterMode(results []sampleResult, mode string) []sampleResult {
	var filtered []sampleResult
	for _, result := range results {
		if result.Config.Mode == mode {
			filtered = append(filtered, result)
		}
	}
	return filtered
}

func sampleValues(results []sampleResult, value func(sampleResult) float64) []float64 {
	values := make([]float64, 0, len(results))
	for _, result := range results {
		values = append(values, value(result))
	}
	sort.Float64s(values)
	return values
}

func median(values []float64) float64 {
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func modeLabel(mode string) string {
	switch mode {
	case "direct-raw", "direct":
		return "direct (raw, no authz)"
	case "direct-auth":
		return "direct (per-service authz)"
	case "mesh":
		return "mesh (1 hop)"
	case "mesh2":
		return "mesh (2 hops)"
	default:
		return mode
	}
}

func observedPositiveMetric(results []sampleResult, mode, source, metric string) bool {
	for _, result := range results {
		if result.Config.Mode != mode {
			continue
		}
		for _, snapshot := range result.MetricSnapshots {
			if snapshot.Source != source {
				continue
			}
			for _, line := range strings.Split(snapshot.Body, "\n") {
				if !strings.HasPrefix(line, metric+"{") && !strings.HasPrefix(line, metric+" ") {
					continue
				}
				fields := strings.Fields(line)
				value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
				if err == nil && value > 0 {
					return true
				}
			}
		}
	}
	return false
}
