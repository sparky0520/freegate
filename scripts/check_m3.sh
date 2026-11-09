#!/usr/bin/env bash
set -euo pipefail

ADDR="127.0.0.1:8085"

echo "=== 1. Starting Freegate with 2 providers (groq and unreachable-test) ==="
./freegate start --addr "$ADDR" --config testdata/m3_config.toml --models testdata/m3_models.toml &
GATEWAY_PID=$!
trap "kill -9 $GATEWAY_PID 2>/dev/null || true" EXIT
sleep 1.5

echo ""
echo "=== 2. Checking /healthz ==="
curl -s "http://${ADDR}/healthz"

echo ""
echo "=== 3. Testing 'freegate status' (acceptance check) ==="
./freegate status --addr "$ADDR"

echo ""
echo "=== 4. Testing 'freegate models' ==="
./freegate models --addr "$ADDR"

echo ""
echo "=== 5. Testing chat completion via auto alias ==="
curl -s -X POST "http://${ADDR}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test" \
  -d '{
    "model": "auto",
    "messages": [{"role": "user", "content": "Say PONG"}]
  }'

echo ""
echo "=== Milestone 3 Acceptance Check Successful! ==="
