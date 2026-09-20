package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// selfProbe implements the container healthcheck.
//
// The runtime image is distroless: no shell, no curl, no wget. Rather than give
// up a static image for the sake of a health probe, the binary probes itself.
// `kilncache -healthcheck` exits 0 when this node's own /readyz says ready.
func selfProbe(args []string) (bool, int) {
	probe := false
	addr := ""
	for _, a := range args {
		switch {
		case a == "-healthcheck" || a == "--healthcheck":
			probe = true
		case strings.HasPrefix(a, "-healthcheck="), strings.HasPrefix(a, "--healthcheck="):
			probe = true
			_, addr, _ = strings.Cut(a, "=")
		}
	}
	if !probe {
		return false, 0
	}

	if addr == "" {
		addr = os.Getenv("KILNCACHE_LISTEN")
	}
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return true, 1
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d: %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		return true, 1
	}
	return true, 0
}
