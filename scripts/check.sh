#!/usr/bin/env bash

# Run all static checks and tests.

cd "$(dirname "$0")/.." || exit 1

failed=0

echo "=== go build ==="
go build ./... || failed=1

echo ""
echo "=== go vet ==="
go vet ./... || failed=1

echo ""
echo "=== gofmt ==="
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "Needs formatting:"
  echo "$unformatted"
  failed=1
else
  echo "OK"
fi

echo ""
echo "=== golangci-lint ==="
if command -v golangci-lint &>/dev/null; then
  golangci-lint run ./... || failed=1
elif [ -x ~/go/bin/golangci-lint ]; then
  ~/go/bin/golangci-lint run ./... || failed=1
else
  echo "SKIP (not installed — go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest)"
fi

echo ""
echo "=== govulncheck ==="
if command -v govulncheck &>/dev/null; then
  govulncheck ./... || failed=1
elif [ -x ~/go/bin/govulncheck ]; then
  ~/go/bin/govulncheck ./... || failed=1
else
  echo "SKIP (not installed — go install golang.org/x/vuln/cmd/govulncheck@latest)"
fi

echo ""
echo "=== go test ==="
go test ./... -count=1 || failed=1

echo ""
if [ "$failed" -eq 0 ]; then
  echo "All checks passed."
else
  echo "Some checks failed."
  exit 1
fi
