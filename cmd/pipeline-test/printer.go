package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	cReset  = "\033[0m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
	cDim    = "\033[2m"
)

type printer struct {
	useColor bool
	start    time.Time
}

func newPrinter(noColor bool) *printer {
	useColor := !noColor
	if useColor {
		if fi, err := os.Stdout.Stat(); err == nil {
			useColor = fi.Mode()&os.ModeCharDevice != 0
		}
	}
	return &printer{useColor: useColor}
}

func (p *printer) color(c, s string) string {
	if !p.useColor {
		return s
	}
	return c + s + cReset
}

func (p *printer) elapsed() string {
	return p.color(cDim, fmt.Sprintf("+%s", time.Since(p.start).Truncate(time.Millisecond)))
}

func (p *printer) header(mode, target, dbStatus string) {
	fmt.Println("=== Rectella Pipeline Test ===")
	fmt.Printf("Mode:     %s\nTarget:   %s\nDatabase: %s\n", mode, target, dbStatus)
	fmt.Println(strings.Repeat("\u2500", 50))
	fmt.Println()
}

func (p *printer) scenarioStart(index, total int, name, desc string) {
	p.start = time.Now()
	fmt.Printf("[%d/%d] %s  %s\n", index, total, name, desc)
}

func (p *printer) send(status int) {
	fmt.Printf("      %s   webhook -> HTTP %d %35s\n", p.color(cCyan, "SEND"), status, p.elapsed())
}

func (p *printer) stage(from, to, detail string) {
	transition := to
	if from != "" {
		transition = from + " -> " + to
	}
	line := fmt.Sprintf("      %s  %-40s %s", p.color(cYellow, "STAGE"), transition, p.elapsed())
	if detail != "" {
		line += "  " + detail
	}
	fmt.Println(line)
}

func (p *printer) check(msg, result string) {
	fmt.Printf("      %s  %-40s %s\n", p.color(cCyan, "CHECK"), msg, result)
}

func (p *printer) pass() { fmt.Printf("      %s\n\n", p.color(cGreen, "PASS")) }

func (p *printer) fail(reason string) {
	fmt.Printf("      %s   %s\n\n", p.color(cRed, "FAIL"), reason)
}

func (p *printer) summary(passed, failed int, elapsed time.Duration, orders []orderResult) {
	fmt.Println(strings.Repeat("\u2500", 50))
	result := fmt.Sprintf("RESULTS  %d passed, %d failed", passed, failed)
	timing := p.color(cDim, fmt.Sprintf("  total %s", elapsed.Truncate(time.Millisecond)))
	if failed > 0 {
		fmt.Println(p.color(cRed, result) + timing)
	} else {
		fmt.Println(p.color(cGreen, result) + timing)
	}
	fmt.Println()
	if len(orders) > 0 {
		fmt.Println("Order Summary:")
		for _, o := range orders {
			fmt.Printf("  %-12s %-10s %-14s %s\n", o.name, o.status, o.sysproOrder, o.skus)
		}
		fmt.Println()
	}
	if failed == 0 {
		fmt.Println("Pipeline: " + p.color(cGreen, "HEALTHY"))
	} else {
		fmt.Println("Pipeline: " + p.color(cRed, "UNHEALTHY"))
	}
}

type orderResult struct {
	name, status, sysproOrder, skus string
}
