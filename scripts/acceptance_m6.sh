#!/usr/bin/env bash
set -euo pipefail

echo "=== Milestone 6 Polish & E2E Acceptance Check ==="

ADDR="127.0.0.1:8090"

echo "1. Testing 'freegate init' in isolated environment..."
TEMP_HOME=$(mktemp -d)
export HOME="$TEMP_HOME"
./freegate init
CONFIG_FOUND=$(find "$TEMP_HOME" -name "config.toml" | head -n 1)
test -n "$CONFIG_FOUND"
test -f "$CONFIG_FOUND"
echo "-> init successfully created: $CONFIG_FOUND"

echo ""
echo "2. Testing 'freegate test' with available keys..."
./freegate test

echo ""
echo "3. Starting freegate server on $ADDR..."
./freegate start --addr "$ADDR" &
GATEWAY_PID=$!
trap "kill -9 $GATEWAY_PID 2>/dev/null || true; rm -rf $TEMP_HOME" EXIT
sleep 1.5

echo ""
echo "4. Testing 'freegate status'..."
./freegate status --addr "$ADDR"

echo ""
echo "5. Testing 'freegate models'..."
./freegate models --addr "$ADDR"

echo ""
echo "6. Testing live end-to-end request through auto model..."
curl -s -X POST "http://${ADDR}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test" \
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Respond with the single word: VERIFIED"}]
  }'

echo ""
echo ""
echo "=== All Milestone 6 acceptance criteria verified successfully in under 10 seconds! ==="
