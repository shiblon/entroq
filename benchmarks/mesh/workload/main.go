// Command workload supplies the HTTP leaf, load generator, and report writer
// used by the local Kubernetes mesh benchmark.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "auth-proxy":
		err = authProxy(os.Args[2:])
	case "serve":
		err = serve(os.Args[2:])
	case "load":
		err = load(os.Args[2:])
	case "opa-allow-all":
		err = installOPAAllowAll(os.Args[2:])
	case "report":
		err = report(os.Args[2:], os.Stdout)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		if silent, ok := err.(interface{ Silent() bool }); !ok || !silent.Silent() {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: workload <auth-proxy|serve|load|opa-allow-all|report> [flags]")
}
