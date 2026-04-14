// Command sku-parity audits SKU parity between a Shopify store and a given
// SYSPRO stock code list. Prints three sections: SKUs only in Shopify
// (dangerous — will fail SORTOI), SKUs only in SYSPRO (not sellable), and
// matches.
//
// Usage:
//
//	go run ./cmd/sku-parity --syspro-skus CBBQ0001,CBBQ0002,LUMP0148
//	go run ./cmd/sku-parity --syspro-skus-file /path/to/skus.txt
//
// Environment (via the usual service env file):
//
//	SHOPIFY_STORE_URL     (e.g. rectella.myshopify.com)
//	SHOPIFY_ACCESS_TOKEN  (shpat_... from the custom app)
//
// Exit codes:
//
//	0 = all SKUs match or the only mismatch is SYSPRO-only (not for sale)
//	1 = at least one Shopify SKU is missing from SYSPRO (would break SORTOI)
//	2 = tool error (credentials, network, flag parsing)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "sku-parity: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	var skusFlag string
	var skusFile string
	flag.StringVar(&skusFlag, "syspro-skus", "", "Comma-separated list of SYSPRO stock codes")
	flag.StringVar(&skusFile, "syspro-skus-file", "", "File containing comma- or newline-separated SYSPRO stock codes")
	flag.Parse()

	storeURL := strings.TrimSpace(os.Getenv("SHOPIFY_STORE_URL"))
	token := strings.TrimSpace(os.Getenv("SHOPIFY_ACCESS_TOKEN"))
	if storeURL == "" || token == "" {
		return fmt.Errorf("SHOPIFY_STORE_URL and SHOPIFY_ACCESS_TOKEN must be set")
	}

	sysproSKUs, err := loadSysproSKUs(skusFlag, skusFile)
	if err != nil {
		return err
	}
	if len(sysproSKUs) == 0 {
		return fmt.Errorf("no SYSPRO SKUs provided (use --syspro-skus or --syspro-skus-file)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	shopifySKUs, err := fetchShopifySKUs(ctx, storeURL, token)
	if err != nil {
		return fmt.Errorf("fetching shopify products: %w", err)
	}

	shopifySet := toSet(shopifySKUs)
	sysproSet := toSet(sysproSKUs)

	var shopifyOnly, sysproOnly, matches []string
	for sku := range shopifySet {
		if sysproSet[sku] {
			matches = append(matches, sku)
		} else {
			shopifyOnly = append(shopifyOnly, sku)
		}
	}
	for sku := range sysproSet {
		if !shopifySet[sku] {
			sysproOnly = append(sysproOnly, sku)
		}
	}
	sort.Strings(shopifyOnly)
	sort.Strings(sysproOnly)
	sort.Strings(matches)

	fmt.Printf("=== SKU parity audit ===\n")
	fmt.Printf("Store: %s\n", storeURL)
	fmt.Printf("SYSPRO SKUs provided: %d\n", len(sysproSKUs))
	fmt.Printf("Shopify SKUs found:   %d\n\n", len(shopifySKUs))

	printSection("Matches (will sync correctly)", matches)
	printSection("Shopify-only (DANGEROUS — SORTOI will reject)", shopifyOnly)
	printSection("SYSPRO-only (not sellable — add to Shopify or ignore)", sysproOnly)

	fmt.Printf("\nSummary: %d matched, %d Shopify-only, %d SYSPRO-only\n",
		len(matches), len(shopifyOnly), len(sysproOnly))

	if len(shopifyOnly) > 0 {
		fmt.Fprintf(os.Stderr, "\nFAIL: %d Shopify SKU(s) not found in SYSPRO stock codes — these would break SORTOI at go-live.\n", len(shopifyOnly))
		os.Exit(1)
	}
	fmt.Println("\nOK: every Shopify SKU has a matching SYSPRO stock code.")
	return nil
}

func loadSysproSKUs(flagVal, fileVal string) ([]string, error) {
	if flagVal != "" && fileVal != "" {
		return nil, fmt.Errorf("pass only one of --syspro-skus or --syspro-skus-file, not both")
	}
	var raw string
	if fileVal != "" {
		b, err := os.ReadFile(fileVal)
		if err != nil {
			return nil, fmt.Errorf("reading SYSPRO SKU file: %w", err)
		}
		raw = string(b)
	} else {
		raw = flagVal
	}
	replacer := strings.NewReplacer("\n", ",", "\r", ",", "\t", ",", " ", ",")
	raw = replacer.Replace(raw)
	var out []string
	for _, tok := range strings.Split(raw, ",") {
		if t := strings.TrimSpace(tok); t != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

func toSet(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[s] = true
	}
	return out
}

func printSection(title string, items []string) {
	fmt.Printf("--- %s (%d) ---\n", title, len(items))
	if len(items) == 0 {
		fmt.Println("  (none)")
	} else {
		for _, s := range items {
			fmt.Printf("  %s\n", s)
		}
	}
	fmt.Println()
}

// fetchShopifySKUs paginates through all products and collects unique
// variant SKUs via cursor-based pagination on the Admin REST API.
func fetchShopifySKUs(ctx context.Context, storeURL, token string) ([]string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	base := fmt.Sprintf("https://%s/admin/api/2025-04", storeURL)

	q := url.Values{}
	q.Set("limit", "250")
	q.Set("fields", "id,variants")
	pageURL := base + "/products.json?" + q.Encode()

	seen := make(map[string]bool)
	for pageURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Shopify-Access-Token", token)
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
		resp.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("shopify HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var payload listProductsResponse
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("parsing response: %w", err)
		}
		for _, p := range payload.Products {
			for _, v := range p.Variants {
				if s := strings.TrimSpace(v.SKU); s != "" {
					seen[s] = true
				}
			}
		}

		// Follow Shopify's Link header for cursor pagination.
		pageURL = parseNextLink(resp.Header.Get("Link"))
	}

	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out, nil
}

type listProductsResponse struct {
	Products []shopifyProduct `json:"products"`
}

type shopifyProduct struct {
	ID       int64                   `json:"id"`
	Variants []shopifyProductVariant `json:"variants"`
}

type shopifyProductVariant struct {
	SKU string `json:"sku"`
}

// parseNextLink extracts the rel="next" URL from a Shopify Link header, if present.
// Format: <https://.../products.json?page_info=...&limit=250>; rel="next"
func parseNextLink(h string) string {
	if h == "" {
		return ""
	}
	for _, part := range strings.Split(h, ",") {
		seg := strings.TrimSpace(part)
		if !strings.Contains(seg, `rel="next"`) {
			continue
		}
		start := strings.Index(seg, "<")
		end := strings.Index(seg, ">")
		if start == -1 || end == -1 || end <= start+1 {
			continue
		}
		return seg[start+1 : end]
	}
	return ""
}
