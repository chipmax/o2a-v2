// Command gatecheck probes the free-tier gates through one live WARP exit: it
// sends an accepted baseline and one single-dimension violation per gate, so an
// upstream rule change is visible instead of inferred from an opaque 429.
//
//	go run ./cmd/gatecheck -source 2603:c024:4526:9797::/64
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"opencode2api/internal/gatecheck"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/warp"
)

func main() {
	source := flag.String("source", "", "outer IPv6 /64 prefix the exit binds its socket to (required)")
	model := flag.String("model", warp.DefaultLaneProbeModel, "free model the probes ask for")
	url := flag.String("url", "https://opencode.ai/inference/v1/chat/completions", "free-lane endpoint")
	tries := flag.Int("tries", 3, "fresh exits to try until the baseline is accepted")
	timeout := flag.Duration("timeout", 45*time.Second, "per-probe deadline")
	flag.Parse()

	prefix, err := netip.ParsePrefix(*source)
	if err != nil || !prefix.Addr().Is6() {
		fmt.Fprintf(os.Stderr, "gatecheck: -source must be an IPv6 prefix, got %q\n", *source)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := warp.NewWithOptions(ctx, logger, warp.Options{Size: 1, Stack: warp.StackIPv6, SourceIPv6: prefix})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gatecheck: %v\n", err)
		os.Exit(2)
	}
	defer pool.Close()

	lease := admit(ctx, pool, *url, *model, *tries, *timeout)
	if lease == nil {
		fmt.Fprintln(os.Stderr, "gatecheck: every fresh exit refused the baseline; the egress blocks are spent, not the contract")
		os.Exit(2)
	}
	defer lease.Release()
	fmt.Printf("egress %s\n\n", lease.EgressKey())

	report := gatecheck.Run(ctx, lease.DialContext, lease.EgressKey(), *model, *url, *timeout, func(format string, args ...any) {
		fmt.Printf("  "+format+"\n", args...)
	})
	fmt.Printf("\nbinding gates: %s\n", strings.Join(report.Enforced, ", "))
	if report.Mismatched > 0 {
		fmt.Printf("contract moved: %d probe(s) differed from the last measurement\n", report.Mismatched)
		os.Exit(1)
	}
}

// admit finds one exit whose baseline is accepted. A refused exit is reported
// to the pool so its egress is parked, then the next fresh exit is tried.
func admit(ctx context.Context, pool *warp.Pool, url, model string, tries int, timeout time.Duration) *warp.Lease {
	for attempt := 1; attempt <= tries; attempt++ {
		lease, err := pool.Acquire(ctx)
		if err != nil {
			return nil
		}
		client := gatecheck.NewClient(lease.DialContext, timeout)
		status, _ := gatecheck.Send(ctx, client, url, gatecheck.BaselineBody(model), identity.CanonicalSessionID("gatecheck-baseline"), httpx.V2UserAgent(), timeout)
		client.CloseIdleConnections()
		fmt.Printf("exit %d: baseline %d\n", attempt, status)
		if status == http.StatusOK {
			return lease
		}
		pool.Report(lease, status, nil)
		lease.Release()
	}
	return nil
}
