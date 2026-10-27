#!/usr/bin/env bash
set -euo pipefail

image="${1:?Usage: smoke-gateway.sh IMAGE}"
directory="$(mktemp -d)"
container="teo-ai-smoke-$$"
cleanup() {
  docker rm --force "$container" >/dev/null 2>&1 || true
  rm -rf "$directory"
}
trap cleanup EXIT

openssl rand -hex 32 > "$directory/key"
chmod 0444 "$directory/key"
printf 'X-Teo-Gateway-Key: %s\n' "$(cat "$directory/key")" > "$directory/headers"
chmod 0600 "$directory/headers"
docker run --detach --name "$container" \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  --publish 127.0.0.1::8080 \
  --mount "type=bind,source=$directory/key,target=/run/secrets/gateway_key,readonly" \
  --env TEO_GATEWAY_KEY_FILE=/run/secrets/gateway_key "$image" >/dev/null
address="http://$(docker port "$container" 8080/tcp)"

ready=false
for _ in {1..30}; do
  if docker exec "$container" /usr/local/bin/teo-ai-gateway -healthcheck; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  docker logs "$container"
  echo 'Gateway did not become healthy' >&2
  exit 1
fi

status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' "$address/codex/models")"
[[ "$status" == 401 ]]
body="$(curl --fail --silent --show-error --header "@$directory/headers" "$address/readyz")"
[[ "$body" == '{"status":"ready"}' ]]
echo 'Gateway smoke test passed (health, authentication, non-root read-only runtime)'
