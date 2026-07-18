// Command goload is a lightweight, dependency-free HTTP load probe used in
// lieu of k6 (not installed on the build/ops machine — see README.md in this
// directory for the k6 script kept alongside this for when k6 IS available).
//
// It drives a fixed aggregate request rate against a small set of read-only,
// unauthenticated-safe live-ninja endpoints and reports p50/p90/p99 latency
// plus a status-code breakdown. It intentionally issues NO writes and NO
// authenticated calls beyond deliberately-401 paths, so it is safe to run
// directly against production (per plan.md M7 "Load test" task and
// CLAUDE.md's "production-only, act with care" rule).
//
// Usage:
//
//	go run ./scripts/loadtest/goload.go
//	go run ./scripts/loadtest/goload.go -base https://live.jeremy.ninja -rps 50 -duration 60s
//
// Targets (weighted round-robin, roughly equal split):
//   - GET /healthz              -> expect 200
//   - GET /v1/compat            -> expect 200 once M7's compat endpoint
//     ships (public route per contracts/headers.md); pre-M7-deploy this
//     currently returns 401 (falls through to Fiber's own RequireAuth);
//     404 accepted too. All treated as benign/expected for this probe, not
//     an error, so the script keeps working across the M7 rollout instead
//     of needing to be re-run.
//   - GET /api/v1/me            -> expect 401 or 403 (no Authorization
//     header sent; observed as 403 {"message":"Forbidden"} from the API
//     Gateway Lambda-authorizer deny-by-default layer, which short-circuits
//     before the web Lambda; RequireAuth()'s own 401 is accepted too for
//     routes that do reach the app — either way it must fail-closed)
//
// Exit code is always 0 (this is a reporting tool, not a test-pass/fail
// gate); read the printed report and the accompanying CloudWatch pull to
// judge pass/fail per plan.md's acceptance bar (ConsumedReadCapacityUnits
// flat, 0 http 5xx, reasonable p99).
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type target struct {
	name       string
	method     string
	path       string
	wantStatus []int // any of these is "expected"; anything else counts as unexpected
}

type result struct {
	target   string
	status   int
	err      error
	duration time.Duration
}

func main() {
	base := flag.String("base", "https://live.jeremy.ninja", "base URL of the live-ninja web app")
	rps := flag.Int("rps", 50, "target aggregate requests per second across all targets")
	duration := flag.Duration("duration", 60*time.Second, "total run duration")
	timeout := flag.Duration("timeout", 10*time.Second, "per-request timeout")
	flag.Parse()

	targets := []target{
		{name: "healthz", method: http.MethodGet, path: "/healthz", wantStatus: []int{200}},
		// /v1/compat doesn't exist as a route yet (pre-M7-deploy): today it
		// falls through to Fiber's own RequireAuth and returns 401
		// {"error":"unauthorized"}; once M7 ships the real GET /v1/compat
		// (a public route per contracts/headers.md) this becomes 200; 404
		// is accepted too in case routing is reshaped in between.
		{name: "v1-compat", method: http.MethodGet, path: "/v1/compat", wantStatus: []int{200, 401, 404}},
		// /api/v1/me is denied at the API Gateway Lambda-authorizer layer
		// (DefaultAuthorizer, deny-by-default) before reaching the web
		// Lambda at all when no credentials are sent, observed as HTTP 403
		// {"message":"Forbidden"} (API Gateway's own boilerplate, not
		// Fiber's). 401 is also accepted since Fiber's own RequireAuth
		// middleware (internal/webapp/middleware.go) independently returns
		// 401 {"error":"unauthorized"} for the same "no credentials" case
		// on routes that do reach the app.
		{name: "api-v1-me-401", method: http.MethodGet, path: "/api/v1/me", wantStatus: []int{401, 403}},
	}

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 200,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}

	interval := time.Second / time.Duration(*rps)
	totalReqs := int(duration.Seconds()) * (*rps)

	fmt.Printf("goload: base=%s rps=%d duration=%s targets=%d (est. %d requests)\n",
		*base, *rps, *duration, len(targets), totalReqs)
	for _, t := range targets {
		fmt.Printf("  - %s %s (expect %v)\n", t.method, t.path, t.wantStatus)
	}

	resultsCh := make(chan result, totalReqs+*rps)
	var wg sync.WaitGroup

	start := time.Now()
	deadline := start.Add(*duration)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	i := 0
	for now := range ticker.C {
		if now.After(deadline) {
			break
		}
		t := targets[i%len(targets)]
		i++
		wg.Add(1)
		go func(t target) {
			defer wg.Done()
			resultsCh <- doRequest(client, *base, t)
		}(t)
	}
	wg.Wait()
	close(resultsCh)

	elapsed := time.Since(start)

	// Aggregate.
	type agg struct {
		durations  []time.Duration
		statusCnt  map[int]int
		unexpected int
		errs       int
	}
	perTarget := map[string]*agg{}
	overall := &agg{statusCnt: map[int]int{}}
	for _, t := range targets {
		perTarget[t.name] = &agg{statusCnt: map[int]int{}}
	}
	wantByName := map[string][]int{}
	for _, t := range targets {
		wantByName[t.name] = t.wantStatus
	}

	total := 0
	for r := range resultsCh {
		total++
		a := perTarget[r.target]
		overall.durations = append(overall.durations, r.duration)
		a.durations = append(a.durations, r.duration)
		if r.err != nil {
			a.errs++
			overall.errs++
			continue
		}
		a.statusCnt[r.status]++
		overall.statusCnt[r.status]++
		if !contains(wantByName[r.target], r.status) {
			a.unexpected++
			overall.unexpected++
		}
	}

	fmt.Printf("\n=== goload report ===\n")
	fmt.Printf("wall time:        %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("total requests:   %d\n", total)
	fmt.Printf("achieved rps:     %.1f\n", float64(total)/elapsed.Seconds())
	fmt.Printf("errors (no resp): %d\n", overall.errs)
	fmt.Printf("unexpected status:%d\n", overall.unexpected)
	p50, p90, p99 := percentiles(overall.durations)
	fmt.Printf("overall latency:  p50=%s p90=%s p99=%s\n", p50, p90, p99)
	fmt.Printf("overall status counts: %v\n", overall.statusCnt)

	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := perTarget[name]
		p50, p90, p99 := percentiles(a.durations)
		fmt.Printf("\n-- %s --\n", name)
		fmt.Printf("  requests:        %d\n", len(a.durations))
		fmt.Printf("  errors (no resp):%d\n", a.errs)
		fmt.Printf("  unexpected:      %d\n", a.unexpected)
		fmt.Printf("  status counts:   %v\n", a.statusCnt)
		fmt.Printf("  latency:         p50=%s p90=%s p99=%s\n", p50, p90, p99)
	}

	if overall.errs > 0 || overall.unexpected > 0 {
		fmt.Fprintf(os.Stderr, "\nNOTE: non-zero errors/unexpected statuses above — inspect before treating the run as clean.\n")
	}
}

func doRequest(client *http.Client, base string, t target) result {
	req, err := http.NewRequest(t.method, base+t.path, nil)
	if err != nil {
		return result{target: t.name, err: err}
	}
	req.Header.Set("User-Agent", "live-ninja-goload/1.0 (+plan.md M7 load probe)")
	start := time.Now()
	resp, err := client.Do(req)
	d := time.Since(start)
	if err != nil {
		return result{target: t.name, err: err, duration: d}
	}
	defer resp.Body.Close()
	return result{target: t.name, status: resp.StatusCode, duration: d}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func percentiles(ds []time.Duration) (p50, p90, p99 time.Duration) {
	if len(ds) == 0 {
		return 0, 0, 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := func(p float64) time.Duration {
		i := int(p * float64(len(sorted)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(sorted) {
			i = len(sorted) - 1
		}
		return sorted[i]
	}
	return idx(0.50), idx(0.90), idx(0.99)
}
