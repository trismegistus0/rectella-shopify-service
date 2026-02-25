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
echo "=== go test ==="
go test ./... -count=1 || failed=1

echo ""
if [ "$failed" -eq 0 ]; then
  echo "All checks passed."
else
  echo "Some checks failed."
  exit 1
fi
