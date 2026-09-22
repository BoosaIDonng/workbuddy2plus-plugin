# WorkBuddy Plugin for CLIProxyAPI

A [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) native plugin
that exposes **Tencent CodeBuddy** (`www.codebuddy.cn` CN and `workbuddy.ai`
Global) as an OAuth provider — with per-account model discovery, a full streaming
executor, credit-aware account routing, four daily automation tasks, and a
built-in seven-tab management panel.

[中文文档 → README_CN.md](README_CN.md)

> **This is a second-development (二改) fork, not the upstream plugin.**
> Upstream `Sliverkiss/cpa-plugin` stopped maintenance at v0.9.3. This project
> ports the superior [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)
> core (SSE normalization, weighted account pool, growth tasks) into the plugin
> and continues development independently.

## Why this fork

The upstream plugin's executor produced the "conversation randomly breaks /
model stops thinking" class of bugs, because its SSE pipeline dropped and
reordered frames. This fork replaces that pipeline wholesale with
workbuddy2api's frame normalizer:

| Problem in upstream | Fix here |
|---|---|
| Conversation breaks mid-stream, thinking content lost | Whitelist frame rebuild, error-frame passthrough, `tool_calls` name convergence by index, id continuation |
| No account-level routing | Three-factor weighted pick (credit ratio, credit expiry, idle compensation) with per-model 6004 cooldown |
| Dead weight in the binary | ~1700 lines of unused scheduler/redisstore/session code deleted |
| No growth automation | Activity map, cat travel, token keepalive, daily check-in |

## Features

### Provider and executor

- **OAuth login** — multi-account `workbuddy-<uid>.json` auth files, written
  through the host's auth store (`host.auth.save`). CN and Global realms share
  one plugin and one config block. Login is driven from CPA's own UI; the panel
  imports credentials rather than running its own login flow.
- **Dynamic model catalog** — each account's entitlements are discovered from
  WorkBuddy and cached per account; missing metadata is enriched from
  [models.dev](https://models.dev). An optional authoritative `models` list can
  replace discovery entirely. Host `oauth-model-alias` /
  `oauth-excluded-models` still apply on top.
- **Streaming executor** — OpenAI-compatible chat completions, both streaming
  (real SSE via `host.stream.emit`) and non-streaming (SSE folded into one
  completion). Includes `tool_choice` normalization, Claude Code template
  sanitization, per-realm system-message injection, and `prompt_cache_key`
  injection.
- **Token counting** — `executor.count_tokens` is implemented.

### Account pool and routing

- **Three-factor weighted pick** (`scheduler_mode: credits`) — ranks candidates
  by remaining-credit ratio, how soon credits expire, and how long the account
  has been idle, then takes a Top-5 shortlist with a 100 ms anti-herd gap.
- **Per-model 6004 cooldown** — a model-specific rate limit cools only that
  model on that account, aligned to the upstream reset wall-clock; other models
  on the same account stay usable.
- **Three-strike session-death handling** — a single 12153 (usually network
  jitter) no longer kills an account; it is disabled only after three
  consecutive failures.
- **Circuit breaker** — 3 consecutive 5xx open a 30-minute breaker with
  exponential backoff up to 6 hours.
- **Hard-credit cooldown** — an exhausted account cools until the next 04:00,
  so the 09:00 check-in can restore it, instead of being re-selected and
  failing repeatedly.

### Daily automation (four tasks)

| Task | Schedule (local) | What it does |
|---|---|---|
| **Check-in** | 09:00, 21:00 | CN daily check-in, then lifecycle reconcile |
| **Token keepalive** | 22:00 | Refreshes access tokens so Keycloak offline sessions do not expire |
| **Activity map** | 10:00 | Sends 5 activity reports per CN account, reads the streak back as a self-check, then claims the reward chain (gift/compensation packs → makeup card → streak tier redeem → lottery draws) |
| **Cat travel** | 09:00, 21:00 | Adopts a buddy when there is none, otherwise advances travel one step (depart when idle, claim when arrived) |

Every task is individually switchable (`checkin_auto`, `token_keepalive`,
`activity_auto`, `travel_auto`) and can be triggered by hand from the panel.
The two long sweeps carry an in-flight guard, so a double-click returns
`409` instead of running the report storm twice.

### Credit lifecycle

| State | CN account | Global account |
|---|---|---|
| Credits > 0 | active | active |
| Credits = 0 | `disabled: true` (auth file kept) | auth file **deleted** |
| Check-in restores credits | re-enabled | n/a (already deleted) |
| Trial available | n/a | claimable once per account |
| Credits unknown | untouched (never mis-kill) | untouched |

Hard credit errors from the executor (402, "insufficient credits", "积分不足")
trigger an immediate reconcile of the failing account.

### Management panel

Seven tabs, served at `/v0/resource/plugins/workbuddy/panel`:

- **总览 Overview** — account counts by state, credit totals, today's check-in
  progress, and a `needs_attention` list.
- **用量 Usage** — official billing data (`get-user-request-usage`) aggregated
  by model, day (Shanghai natural day) and client, plus a dependency-free SVG
  trend chart over a preset or custom date range.
- **账号 Accounts** — per-account credit progress bars, credit-expiry
  countdown, plan badges, check-in state with streak, region filter, search,
  sort, manual check-in, trial claim, credential import, and a shelf/restore
  toggle that pulls an account out of routing without deleting it.
- **请求日志 Request log** — every executor outcome with model, status, TTFB,
  latency, tokens and a redacted error.
- **任务记录 Tasks** — every automation run with its result and the credits it
  granted, plus a running total.
- **模型中心 Models** — the full upstream catalog: display name, description,
  context window, max output, reasoning effort levels, credit multiplier,
  tags and capabilities.
- **设置 Settings** — automation toggles, keepalive status, prompt
  desensitization terms, and connection info.

The panel is served from CPA's origin and reads the host's management key from
same-origin `localStorage` (or a `?key=` parameter), then forwards it. The
plugin enforces no key of its own.

## Quickstart

### 1. Install

Drop the compiled `workbuddy.so` into CPA's plugin directory using the
platform convention:

```
plugins/
  linux/amd64/workbuddy-v2.8.0.so
  linux/arm64/workbuddy-v2.8.0.so
```

Only one file may resolve to plugin id `workbuddy` at a time — the id is the
auth-type routing key, so two copies cannot be loaded in parallel.

### 2. Enable in `config.yaml`

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    workbuddy:
      enabled: true
```

### 3. Sign in

Add accounts through CPA's own login UI. The plugin writes one
`workbuddy-<uid>.json` per account to the auth store. Credentials exported from
another deployment can also be pasted into the panel's import dialog.

### 4. Use it

```bash
curl http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5.3-flash",
    "messages": [{"role": "user", "content": "hi"}],
    "stream": true
  }'
```

## Configuration

All fields are optional and live under `plugins.configs.workbuddy`.

```yaml
plugins:
  configs:
    workbuddy:
      enabled: true

      # Optional authoritative model ID list. Entries must be single-line YAML
      # strings. A non-empty list is the complete catalog: WorkBuddy catalog
      # HTTP and cache access are bypassed, while models.dev metadata fetch,
      # ETag and last-good cache behaviour stays active.
      # Missing, null, or [] keeps dynamic WorkBuddy discovery.
      models: []

      # Daily check-in for CN accounts (default true). 09:00 and 21:00 local.
      checkin_auto: true

      # Credit lifecycle: disable CN on exhaust, delete Global on exhaust,
      # re-enable CN after check-in restores credits (default true).
      lifecycle_auto: true

      # Daily access-token refresh at 22:00 local (default true), so Keycloak
      # offline sessions do not expire.
      token_keepalive: true

      # Daily activity-map task at 10:00 local (default true): activity
      # reports, streak self-check, growth reward claims.
      activity_auto: true

      # Daily cat-travel task at 09:00 and 21:00 local (default true):
      # buddy adoption, departure, arrival reward.
      travel_auto: true

      # Insert U+200B into configured blocked terms in system/developer prompt
      # text and tool title/description fields (default false).
      desensitize: false

      # Editable literal term list for desensitize. Missing uses the built-in
      # 85-term list; [] means an empty custom list.
      desensitize_terms: []

      # OAuth request profile: cli (default) or workbuddy (desktop profile).
      oauth_client_mode: "cli"

      # Probe strict CN enterprise credits before personal resource packages
      # (default false; Global unchanged).
      enterprise_credits: false

      # Account selection (default "off"):
      #   off     → defer entirely to CPA's built-in scheduler
      #   credits → plugin orders the candidates by three-factor weight
      scheduler_mode: "off"
```

**Authorization** belongs to the CPA host: its `remote-management` middleware
requires the management key on every `/v0/management/*` request and bans an IP
after repeated failures. The plugin deliberately enforces no key of its own —
a second key would reject the host's own key, which is the only credential the
panel can obtain automatically.

**Proxy** settings are not supported at the plugin level. All outbound requests
follow the host's routing. A stale `proxy-url` line fails loudly at reconfigure
rather than being silently ignored.

Model aliases and exclusions are handled natively by CPA's `oauth-model-alias`
and `oauth-excluded-models` — no plugin-side duplication.

## Model catalog

The catalog is per account, and its readiness gates execution. Only `ready` and
`stale` accounts are executable; `not_started`, `loading` and `failed` are
rejected at every executor entry point with a fixed, redacted `not_ready`
response (HTTP 503) and are also excluded from scheduling.

In dynamic mode the first `model.for_auth` call for an account is the
authenticated bootstrap boundary:

1. WorkBuddy `GET /v3/config` supplies the account's entitled model IDs and
   serving fields. Only an HTTP 404 or 405 falls back to the legacy
   `GET /console/enterprises/personal/models`; other failures do not.
2. [models.dev `/models.json`](https://models.dev/models.json) supplies
   canonical metadata for fields WorkBuddy omitted. It never determines
   entitlement or overrides WorkBuddy serving fields.
3. Responses are validated before replacing the persistent cache, and an
   immutable per-account catalog is published.

A first bootstrap with no valid cache is fail-closed: both sources must fetch,
validate and persist before the account becomes `ready`. Later starts still
attempt both refreshes; if a refresh fails but that source has a valid
last-good cache, the account starts `stale` on that cache. A source with
neither leaves the account `failed`.

Cache location (`os.UserConfigDir()`, so `/root/.config/...` for root on Linux):

```plaintext
<user-config-dir>/CLIProxyAPI/workbuddy/model-catalog/
  metadata.json
  metadata.json.bak
  models/
    <identity-sha256>.json
    <identity-sha256>.json.bak
```

Panel observability data (request log, credit ledger, task records, pool state)
lives beside it under `<user-config-dir>/CLIProxyAPI/workbuddy/data/`, or in
`$WB2_DATA_DIR` when set. Each stream is capped at 2000 rows and written with a
debounced atomic tmp+rename.

## Panel API

All endpoints sit under `<MANAGEMENT_BASE_PATH>/plugins/workbuddy` and require
the host's management key.

| Method | Path | Purpose |
|---|---|---|
| GET | `/accounts` | Account list (light load: cached values only) |
| POST | `/refresh` | Force refresh + lifecycle reconcile |
| GET | `/overview` | Aggregate tiles + needs-attention |
| GET | `/usage?days=1..31` | Billing aggregation |
| GET | `/credits[?auth_index=]` | Credits (and check-in) for one or all accounts |
| GET | `/requests?limit=` | Request log |
| GET | `/ledger?limit=` | Credit ledger |
| GET | `/tasks?limit=` | Task records |
| GET | `/models` | Full upstream model catalog |
| GET | `/keepalive/status` | Keepalive schedule + last run |
| GET | `/egress-ip` | Egress IP |
| GET | `/desensitize` | Effective desensitize settings |
| POST | `/checkin` | Check in one account or all |
| POST | `/checkin/config` | Toggle auto check-in |
| POST | `/keepalive` | Refresh tokens now |
| POST | `/import` | Import credential JSON |
| POST | `/trial` | Claim Global expert trial |
| POST | `/select` | Set the panel-selected account |
| POST | `/account/toggle` | Shelf/restore an account |
| POST | `/activity` | Run the activity sweep now (409 if already running) |
| POST | `/travel` | Run the travel sweep now (409 if already running) |

Errors are returned as `{"error": "<redacted message>"}`. Credentials, tokens
and raw upstream bodies never appear in a response.

## Development

Requires Go 1.26+ (matches CPA) and Node for the panel tests.

```bash
make build     # CGO_ENABLED=1 go build -buildmode=c-shared -o workbuddy.so .
make test      # go test -race -count=1 ./... && node --test panel.test.js
make lint      # gofmt -l . && go vet ./...
make release   # cross-build linux/amd64 + linux/arm64 into dist/
```

Building on Windows via Git Bash needs the path-conversion workaround:

```bash
MSYS_NO_PATHCONV=1 docker run --rm \
  -v "//d/path/to/workbuddy2":/src -w //src golang:1.26-bookworm \
  sh -c 'CGO_ENABLED=1 go build -buildmode=c-shared -o workbuddy.so .'
```

See [docs/architecture.md](docs/architecture.md) for the module map and
[docs/development.md](docs/development.md) for the full workflow.

## Compatibility

- **CPA**: built against `CLIProxyAPI/v7` SDK, plugin ABI version 1.
- **Realm isolation**: a CN account and a Global account must not be logged in
  through two different clients at once — Keycloak sessions are single-point,
  and refresh tokens invalidate each other.

## License

MIT — see [LICENSE](LICENSE). Upstream plugin © Sliverkiss; this fork's
modifications are released under the same terms.
