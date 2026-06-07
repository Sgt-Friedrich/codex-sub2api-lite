# codex-sub2api-lite

Tiny local Codex CLI proxy for Sub2API OpenAI OAuth account exports.

It is intentionally not a full Sub2API replacement. It only does the narrow path
needed by Codex CLI:

- scan a folder for `account_*_sub2api_cpa.zip` or `sub2api_*.json`
- load OpenAI OAuth `access_token` account entries
- expose local OpenAI-compatible `/v1/responses`
- forward to `https://chatgpt.com/backend-api/codex/responses`
- rotate accounts on `401`, `429`, or upstream `5xx`

No PostgreSQL, Redis, web UI, billing, or admin frontend is used.

## Memory Target

The service is written in Go standard library only. On Linux it normally stays
around the low tens of MB RSS. The supplied systemd unit sets:

```ini
MemoryMax=50M
```

For stricter experiments, try `MemoryMax=30M`. `20M` may work on some systems
but is not guaranteed for TLS streaming workloads.

## File Layout

Recommended server layout:

```text
/root/codex-sub2api/
  codex-sub2api-lite
  accounts/
    account_385_sub2api_cpa.zip
    account_386_sub2api_cpa.zip
    ...
```

After startup, new files dropped into `accounts/` are picked up every 30 seconds.

Supported account files:

- `account_*_sub2api_cpa.zip`, containing `sub2api_*.json` and optional `cpa/token_*.json`
- `sub2api_*.json` directly in the `accounts/` directory

The service only reads OpenAI OAuth account entries from those files. Keep the
directory private because these files contain account tokens.

## Run

```bash
CS2API_ACCOUNTS_DIR=/root/codex-sub2api/accounts \
CS2API_LISTEN=127.0.0.1:8787 \
CS2API_API_KEY=sk-local-change-me \
/root/codex-sub2api/codex-sub2api-lite
```

Check:

```bash
curl http://127.0.0.1:8787/health
curl -H "Authorization: Bearer sk-local-change-me" http://127.0.0.1:8787/accounts
curl http://127.0.0.1:8787/v1/models
```

## Systemd Deployment

Build on any machine with Go 1.22+:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags="-s -w" \
  -o codex-sub2api-lite ./cmd/codex-sub2api-lite
```

Install on the server:

```bash
install -d -m 700 /root/codex-sub2api/accounts
install -m 755 codex-sub2api-lite /root/codex-sub2api/codex-sub2api-lite
install -m 644 codex-sub2api-lite.service /etc/systemd/system/codex-sub2api-lite.service
```

Create the local API key file:

```bash
key="sk-local-$(tr -dc A-Za-z0-9 </dev/urandom | head -c 32)"
printf 'CS2API_API_KEY=%s\nSUB2API_API_KEY=%s\n' "$key" "$key" \
  > /root/codex-sub2api/env
chmod 600 /root/codex-sub2api/env
```

Start:

```bash
systemctl daemon-reload
systemctl enable --now codex-sub2api-lite
systemctl status codex-sub2api-lite
```

The included unit binds to `127.0.0.1:8787`, limits memory with
`MemoryMax=50M`, and uses a local HTTP proxy at `127.0.0.1:7890` if present.
Remove the proxy environment lines if the server can reach ChatGPT directly.

## Codex CLI

Add a custom provider to `~/.codex/config.toml`:

```toml
model = "gpt-5.3-codex"
model_provider = "sub2api-lite"

[model_providers.sub2api-lite]
name = "sub2api-lite"
base_url = "http://127.0.0.1:8787/v1"
wire_api = "responses"
env_key = "SUB2API_API_KEY"
```

Then:

```bash
cat >/etc/profile.d/codex-sub2api-lite.sh <<'EOF'
if [ -f /root/codex-sub2api/env ]; then
  set -a
  . /root/codex-sub2api/env
  set +a
fi
EOF

echo '. /etc/profile.d/codex-sub2api-lite.sh' >> /root/.bashrc
codex
```

After this, opening a new SSH session and typing `codex` uses the local
`sub2api-lite` provider by default.

## Security

- Bind to `127.0.0.1` only.
- Keep account ZIPs private; they contain OAuth tokens.
- Do not commit account ZIPs, token JSON files, or real API keys.
- Use a local API key even when bound to localhost.

## Limitations

- It does not refresh expired OAuth tokens yet.
- It does not implement Sub2API billing, users, groups, or web UI.
- It focuses on Codex Responses streaming, not general OpenAI API coverage.
