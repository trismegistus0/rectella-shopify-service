// Command benchmark measures round-trip latency for SYSPRO e.net business
// object operations against the live RILT test company. It tests INVQRY,
// SORQRY, and SORTOI across multiple iterations and reports min/max/avg/p95.
//
// Usage:
//
//	go run ./cmd/benchmark [--iterations N]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

type result struct {
	op       string
	duration time.Duration
	err      error
}

type stats struct {
	op    string
	count int
	errs  int
	min   time.Duration
	max   time.Duration
	avg   time.Duration
	p95   time.Duration
}

func main() {
	iterations := flag.Int("iterations", 5, "number of iterations per operation")
	flag.Parse()

	if *iterations < 1 {
		fmt.Fprintln(os.Stderr, "FAIL: iterations must be >= 1")
		os.Exit(1)
	}

	if err := run(*iterations); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(iterations int) error {
	baseURL := requireEnv("SYSPRO_ENET_URL")
	operator := requireEnv("SYSPRO_OPERATOR")
	password := os.Getenv("SYSPRO_PASSWORD") // blank password is valid
	companyID := requireEnv("SYSPRO_COMPANY_ID")

	baseURL = strings.TrimRight(baseURL, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	ctx := context.Background()

	fmt.Println("SYSPRO e.net Benchmark")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Printf("Endpoint:   %s\n", baseURL)
	fmt.Printf("Operator:   %s\n", operator)
	fmt.Printf("Company:    %s\n", companyID)
	fmt.Printf("Iterations: %d per operation\n", iterations)
	fmt.Println()

	// Logon (timed separately, not part of per-op benchmarks)
	fmt.Print("Logging in... ")
	loginStart := time.Now()
	guid, err := logon(ctx, client, baseURL, operator, password, companyID)
	loginDur := time.Since(loginStart)
	if err != nil {
		return fmt.Errorf("logon failed: %w", err)
	}
	fmt.Printf("OK (%s)\n\n", loginDur.Round(time.Millisecond))

	defer func() {
		fmt.Print("Logging off... ")
		if err := logoff(ctx, client, baseURL, guid); err != nil {
			fmt.Printf("WARN: %v\n", err)
		} else {
			fmt.Println("OK")
		}
	}()

	totalStart := time.Now()

	// Collect results per operation type
	results := make(map[string][]result)

	// --- INVQRY ---
	invXml := `<Query><Key><StockCode>CBBQ0001</StockCode></Key></Query>`
	fmt.Printf("Running INVQRY x%d", iterations)
	for i := 0; i < iterations; i++ {
		fmt.Print(".")
		start := time.Now()
		params := url.Values{
			"UserId":         {guid},
			"BusinessObject": {"INVQRY"},
			"XmlIn":          {invXml},
		}
		_, err := doGet(ctx, client, baseURL+"/Query/Query", params)
		dur := time.Since(start)
		results["INVQRY"] = append(results["INVQRY"], result{op: "INVQRY", duration: dur, err: err})
	}
	fmt.Println(" done")

	// --- SORQRY ---
	sorqryXml := `<Query><Key><SalesOrderNumber>000001</SalesOrderNumber></Key><Option><IncludeStockedLines>N</IncludeStockedLines><IncludeNonStockedLines>N</IncludeNonStockedLines><IncludeFreightLines>N</IncludeFreightLines><IncludeMiscLines>N</IncludeMiscLines><IncludeCommentLines>N</IncludeCommentLines></Option></Query>`
	fmt.Printf("Running SORQRY x%d", iterations)
	for i := 0; i < iterations; i++ {
		fmt.Print(".")
		start := time.Now()
		params := url.Values{
			"UserId":         {guid},
			"BusinessObject": {"SORQRY"},
			"XmlIn":          {sorqryXml},
		}
		_, err := doGet(ctx, client, baseURL+"/Query/Query", params)
		dur := time.Since(start)
		results["SORQRY"] = append(results["SORQRY"], result{op: "SORQRY", duration: dur, err: err})
	}
	fmt.Println(" done")

	// --- SORTOI ---
	sortoiParams := `<SalesOrders><Parameters><IgnoreWarnings>Y</IgnoreWarnings><AlwaysUsePriceEntered>Y</AlwaysUsePriceEntered><AllowZeroPrice>Y</AllowZeroPrice></Parameters></SalesOrders>`
	fmt.Printf("Running SORTOI x%d", iterations)
	for i := 0; i < iterations; i++ {
		fmt.Print(".")
		ts := time.Now().Format("20060102-150405")
		sortoiXml := fmt.Sprintf(`<SalesOrders><Orders><OrderHeader><CustomerPoNumber>#BENCH-%s</CustomerPoNumber><Customer>WEBS01</Customer><OrderDate>%s</OrderDate></OrderHeader><OrderDetails><StockLine><CustomerPoLine>0001</CustomerPoLine><StockCode>CBBQ0001</StockCode><OrderQty>1</OrderQty><OrderUom>EA</OrderUom><Price>9.99</Price><PriceUom>EA</PriceUom></StockLine></OrderDetails></Orders></SalesOrders>`, ts, time.Now().Format("2006-01-02"))
		start := time.Now()
		params := url.Values{
			"UserId":         {guid},
			"BusinessObject": {"SORTOI"},
			"XmlParameters":  {sortoiParams},
			"XmlIn":          {sortoiXml},
		}
		_, err := doGet(ctx, client, baseURL+"/Transaction/Post", params)
		dur := time.Since(start)
		results["SORTOI"] = append(results["SORTOI"], result{op: "SORTOI", duration: dur, err: err})
	}
	fmt.Println(" done")

	totalElapsed := time.Since(totalStart)

	// Compute and print stats
	fmt.Println()
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println("Results")
	fmt.Println(strings.Repeat("=", 60))
	fmt.Println()

	// Table header
	fmt.Printf("%-10s %6s %8s %8s %8s %8s %6s\n",
		"Operation", "N", "Min", "Avg", "P95", "Max", "Errs")
	fmt.Println(strings.Repeat("-", 60))

	ops := []string{"INVQRY", "SORQRY", "SORTOI"}
	for _, op := range ops {
		s := computeStats(op, results[op])
		fmt.Printf("%-10s %6d %8s %8s %8s %8s %6d\n",
			s.op, s.count,
			fmtMs(s.min), fmtMs(s.avg), fmtMs(s.p95), fmtMs(s.max),
			s.errs)
	}

	fmt.Println(strings.Repeat("-", 60))
	fmt.Printf("\nLogin latency:   %s\n", fmtMs(loginDur))
	fmt.Printf("Total elapsed:   %s\n", fmtMs(totalElapsed))
	fmt.Printf("Operations run:  %d\n", iterations*len(ops))
	fmt.Println()

	return nil
}

func computeStats(op string, results []result) stats {
	s := stats{op: op, count: len(results)}
	if len(results) == 0 {
		return s
	}

	var durations []time.Duration
	var total time.Duration
	s.min = time.Duration(math.MaxInt64)

	for _, r := range results {
		if r.err != nil {
			s.errs++
		}
		durations = append(durations, r.duration)
		total += r.duration
		if r.duration < s.min {
			s.min = r.duration
		}
		if r.duration > s.max {
			s.max = r.duration
		}
	}

	s.avg = total / time.Duration(len(results))

	// P95: sort and pick the 95th percentile value
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95idx := int(math.Ceil(float64(len(durations))*0.95)) - 1
	if p95idx < 0 {
		p95idx = 0
	}
	if p95idx >= len(durations) {
		p95idx = len(durations) - 1
	}
	s.p95 = durations[p95idx]

	return s
}

func fmtMs(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%.0fms", ms)
}

// --- HTTP helpers (same pattern as enettest/sorqrytest) ---

func logon(ctx context.Context, client *http.Client, baseURL, operator, password, companyID string) (string, error) {
	params := url.Values{
		"Operator":         {operator},
		"OperatorPassword": {password},
		"CompanyId":        {companyID},
	}
	body, err := doGet(ctx, client, baseURL+"/Logon", params)
	if err != nil {
		return "", err
	}
	var guid string
	if err := json.Unmarshal(body, &guid); err != nil {
		guid = strings.TrimSpace(string(body))
	}
	if guid == "" {
		return "", fmt.Errorf("logon returned empty GUID (body: %s)", string(body))
	}
	return guid, nil
}

func logoff(ctx context.Context, client *http.Client, baseURL, guid string) error {
	params := url.Values{"UserId": {guid}}
	_, err := doGet(ctx, client, baseURL+"/Logoff", params)
	return err
}

func doGet(ctx context.Context, client *http.Client, target string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func requireEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "FAIL: missing required env var %s\n", key)
		os.Exit(1)
	}
	return v
}
