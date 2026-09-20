// Command kilnchaos drives mixed traffic against a KilnCache cluster while
// stopping and restarting nodes on a schedule, and reports what broke.
//
// The point of this tool is the independence of its bookkeeping. It computes
// the SHA-256 of every object it writes, keeps that value itself, and checks
// every byte it reads back against its own record -- never against anything the
// server says. A corruption check that trusted the server's own digest would
// pass for exactly the bug it is meant to find.
//
// It also insists on reporting sample sizes. "Zero corrupted reads" with no
// count of reads checked is not a claim, and this tool will not emit one.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "kilnchaos: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	nodes          []string
	duration       time.Duration
	workers        int
	readRatio      float64
	objectMin      int
	objectMax      int
	faultEvery     time.Duration
	faultDown      time.Duration
	convergeWait   time.Duration
	out            string
	composeFile    string
	composeProject string
	seed           int64
	noFaults       bool
}

func run(args []string) error {
	fs := flag.NewFlagSet("kilnchaos", flag.ContinueOnError)
	var (
		nodesRaw = fs.String("nodes", "node-a=http://localhost:8080,node-b=http://localhost:8081,node-c=http://localhost:8082",
			"cluster as name=url,name=url; the name must match the container name suffix for fault injection")
		duration     = fs.Duration("duration", 10*time.Minute, "how long to run")
		workers      = fs.Int("workers", 16, "concurrent client goroutines")
		readRatio    = fs.Float64("read-ratio", 0.7, "fraction of operations that are reads")
		objectMin    = fs.Int("object-min", 1024, "smallest object in bytes")
		objectMax    = fs.Int("object-max", 512*1024, "largest object in bytes")
		faultEvery   = fs.Duration("fault-every", 45*time.Second, "how often to stop a node")
		faultDown    = fs.Duration("fault-down", 20*time.Second, "how long a stopped node stays down")
		convergeWait = fs.Duration("converge-wait", 3*time.Minute, "how long to wait for replica convergence after the last fault")
		out          = fs.String("out", "", "directory for the JSON report (required)")
		composeFile  = fs.String("compose-file", "deploy/compose/docker-compose.yml", "compose file used to stop and start nodes")
		composeProj  = fs.String("compose-project", "kilncache", "compose project name")
		seed         = fs.Int64("seed", 0, "RNG seed; 0 means use the clock")
		noFaults     = fs.Bool("no-faults", false, "run traffic without stopping anything (a control run)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out is required: a chaos run whose report is not written is not evidence")
	}

	nodes, err := parseNodes(*nodesRaw)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return errors.New("no nodes given")
	}
	if *seed == 0 {
		*seed = time.Now().UnixNano()
	}

	opts := options{
		nodes:          nodes,
		duration:       *duration,
		workers:        *workers,
		readRatio:      *readRatio,
		objectMin:      *objectMin,
		objectMax:      *objectMax,
		faultEvery:     *faultEvery,
		faultDown:      *faultDown,
		convergeWait:   *convergeWait,
		out:            *out,
		composeFile:    *composeFile,
		composeProject: *composeProj,
		seed:           *seed,
		noFaults:       *noFaults,
	}
	return runChaos(opts)
}

// target is one node this run talks to.
//
// Named "target" rather than "node" so the tool can import internal/node in its
// tests without the two colliding.
type target struct {
	Name string
	URL  string
}

var nodeOrder []target

func parseNodes(s string) ([]string, error) {
	nodeOrder = nil
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("node %q is not in name=url form", part)
		}
		nodeOrder = append(nodeOrder, target{Name: strings.TrimSpace(name), URL: strings.TrimRight(strings.TrimSpace(url), "/")})
	}
	names := make([]string, 0, len(nodeOrder))
	for _, n := range nodeOrder {
		names = append(names, n.Name)
	}
	return names, nil
}
