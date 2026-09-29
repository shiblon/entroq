package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const allowAllPolicy = `package meshbench

import rego.v1

authz := {"allow": true}
`

func installOPAAllowAll(args []string) error {
	flags := flag.NewFlagSet("opa-allow-all", flag.ContinueOnError)
	url := flags.String("url", "http://entroq.entroq-system.svc.cluster.local:8181/v1/policies/meshbench-allow-all", "OPA policy API URL")
	timeout := flags.Duration("timeout", 10*time.Second, "request timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return fmt.Errorf("timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, *url, bytes.NewBufferString(allowAllPolicy))
	if err != nil {
		return fmt.Errorf("create OPA policy request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("put OPA policy: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read OPA policy response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("put OPA policy: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
