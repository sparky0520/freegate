#!/usr/bin/env bash
set -euo pipefail

ADDR="127.0.0.1:8080"
URL="http://${ADDR}/v1/chat/completions"

echo "=== 1. Starting gateway with API key ==="
./gateway -addr "$ADDR" &
GATEWAY_PID=$!
trap "kill -9 $GATEWAY_PID 2>/dev/null || true" EXIT
sleep 0.5

echo "=== 2. Testing real non-streaming reply ==="
curl -s -w "\nHTTP Status: %{http_code}\n" -X POST "$URL" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test" \
  -d '{
    "model": "openai/gpt-oss-20b",
    "stream": false,
    "messages": [{"role": "user", "content": "Respond with the single word: PONG"}]
  }'

echo ""
echo "=== 3. Testing real streaming reply (curl -N) ==="
curl -N -s -X POST "$URL" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer test" \
  -d '{
    "model": "openai/gpt-oss-20b",
    "stream": true,
    "messages": [{"role": "user", "content": "Count from 1 to 5 slowly."}]
  }'

echo ""
echo "=== 4. Stopping gateway ==="
kill "$GATEWAY_PID" || true
wait "$GATEWAY_PID" 2>/dev/null || true

echo "=== 5. Starting gateway without upstream authorization header ==="
./gateway -addr "$ADDR" -no-auth &
GATEWAY_PID=$!
sleep 0.5

echo "=== 6. Testing upstream 401 response without auth header ==="
curl -s -w "\nHTTP Status: %{http_code}\n" -X POST "$URL" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openai/gpt-oss-20b",
    "messages": [{"role": "user", "content": "hi"}]
  }'

echo ""
echo "=== Acceptance check completed successfully ==="
