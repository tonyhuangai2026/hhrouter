# Agent Router Platform

A self-hostable **LLM routing gateway** (inspired by new-api). It exposes a single,
unified inbound surface in **both OpenAI and Anthropic Messages formats**, then routes
each request to an OpenAI-compatible or AWS Bedrock upstream — applying multi-dimensional
routing rules, weighted load balancing with failover, per-key + per-user quotas, and full
request logging with an analytics dashboard.

- **Backend** — Go + Gin + GORM (`/server`)
- **Frontend** — React + Vite + Semi Design (`/web`)
  - 界面语言：中/英，自动检测可手动切换 / UI language: Chinese & English, auto-detected and switchable
- **Data** — PostgreSQL 16 (durable) + Redis 7 (quota counters, model cache)
- **Deploy** — `docker compose up -d` for local/PoC, or **ECS on EC2 + RDS + ElastiCache**
  for a highly available deployment (see [Deployment options](#deployment-options))

---

## Architecture

```
                       ┌──────────── Browser (React + Semi) ────────────┐
                       │  Login/Register · Channels · Routing Rules ·    │
                       │  API Keys · Dashboard · Users                   │
                       └───────────────┬─────────────────────────────────┘
                                       │  HTTP :8080  (nginx)
  Downstream clients  ── sk-xxxx ──┐   │  /         → SPA (static dist)
  (OpenAI / Anthropic SDK)         │   │  /api/*    → backend (admin, JWT)
                                   │   │  /v1/*     → backend (relay, sk- key)
                                   ▼   ▼
   ┌──────────────────── frontend (nginx) reverse proxy ────────────────────┐
   └───────────────────────────────┬────────────────────────────────────────┘
                                    │  backend:3000
                ┌───────────────────▼───────────────────────────┐
                │            Go + Gin server (/server)           │
                │  Admin API /api (JWT)   Relay API /v1 (sk-)    │
                │  middleware: auth · quota · log                │
                │                  │                             │
                │            Router Engine                       │
                │     rules → candidates → LB → failover         │
                │          │                  │                  │
                │   OpenAI Adapter      Bedrock Adapter          │
                └──────────┼──────────────────┼──────────────────┘
                           ▼                  ▼
                 OpenAI-compatible      bedrock-runtime.<region>
                 upstreams (/v1/...)    .amazonaws.com/model/<id>/converse
                           │                  │
            ┌──────────────┴──────────────────┴──────────────┐
            ▼                                                 ▼
      PostgreSQL 16  (users, channels, tokens, rules, logs)   Redis 7
                                                         (quota, model cache)
```

Services in `docker-compose.yml`:

| Service    | Image / build         | Role                                              |
|------------|-----------------------|---------------------------------------------------|
| `postgres` | `postgres:16-alpine`  | Durable store (named volume `pgdata`, healthcheck)|
| `redis`    | `redis:7-alpine`      | Quota counters + model cache (healthcheck)        |
| `backend`  | build `./server`      | Go server; waits for db+redis healthy             |
| `frontend` | build `./web`         | nginx: serves SPA, proxies `/api` + `/v1`         |

The frontend container hosts the built SPA and reverse-proxies `/api` and `/v1` to the
backend, so the browser only ever talks to one origin (`http://localhost:8080`).

---

## Quickstart (Docker)

Requires Docker with the Compose plugin (`docker compose`).

```bash
# 1. Configure
cp .env.example .env
#    Edit .env and set strong JWT_SECRET and SECRET_KEY values.

# 2. Build and start everything
docker compose up -d --build

# 3. Wait for health, then verify
docker compose ps                      # all four services should be healthy
curl -fsS http://localhost:8080/api/ping   # {"message":"pong","db":true,"redis":true}

# Open the UI
open http://localhost:8080             # (or just visit it in your browser)
```

Tear down (keep data) / wipe everything:

```bash
docker compose down            # stop containers, keep volumes
docker compose down -v         # also delete pgdata + redisdata (full reset)
```

> **Ports:** the UI/API are published on `HTTP_PORT` (default `8080`). The backend,
> postgres, and redis are internal to the compose network and not published on the host.

---

## Deployment options

Local `docker compose` is the fastest way to try the platform, but it puts the app and its
data on one machine — lose it and you lose both. Two CloudFormation templates under
[`/deploy`](deploy/README.md) cover the two postures:

| | `docker compose` / `cloudformation.yml` | `cloudformation-ecs.yml` |
|---|---|---|
| **Topology** | one box: app + postgres + redis | ECS on EC2 (Graviton), 2 tasks on separate instances across 2 AZs, ALB in front |
| **Data** | on the box's disk | **RDS PostgreSQL** + **ElastiCache Redis** |
| **A node dies** | outage until it is rebuilt; disk data is lost with the instance | no outage — ECS reschedules onto the spare instance, the ASG replaces the dead one, ALB stops routing to unhealthy targets |
| **Cost (rough)** | ~$16/mo | ~$120/mo (3x c6g.large + NAT + RDS + Redis + ALB) |
| **Good for** | PoC, demo, local dev | anything carrying real traffic |

The application is **stateless** — it writes nothing to local disk, holds no in-process
cache, and signs JWTs with HS256 instead of keeping server-side sessions — so scaling out
needs no code change and no session affinity. The only state that must live outside a
container is PostgreSQL. Redis is not authoritative either: quota counters reseed
themselves from the database's `used_quota` columns and the channel model list is a
10-minute cache, so losing Redis costs seconds of accuracy rather than data.

Two things worth knowing before running behind a load balancer:

- **Raise the idle timeout.** `/v1/*` responses are SSE, and a model that is still
  thinking looks like an idle connection. The ECS template defaults to 300s; the AWS
  default of 60s truncates long answers mid-stream.
- **The ECS template takes an existing `VpcId`.** Bring your own VPC (fresh or existing) —
  [deploy/README.md](deploy/README.md) lists exactly what it must provide and includes a
  script that creates a conforming one.
- **Instances default to Graviton (arm64), so the images must be arm64.** An amd64 image
  will not start on c6g.large; build with `docker buildx --platform linux/arm64`, or switch
  `InstanceType`/`InstanceArchitecture`/`EcsAmiId` to x86.

---

## How routing works

Four steps turn one inbound request into one upstream call.

```
  request ─┬─ group   (whose API key)
           ├─ model   (what the client asked for)
           └─ tokens  (estimated prompt size)
                │
 ═══════════════▼═══ (1) MATCH ══════════════════════════════════════════════
                │   Rules sorted by priority ASCENDING. The FIRST rule whose
                │   every condition holds wins — and that is final: there is
                │   no backtracking to a later rule.
                │
     ┌───────────────────────────────┐
     │ rule 0   match + expr         │
     │ rule 1   match + expr         │
     │ rule 2   (catch-all, no expr) │
     └───────────────────────────────┘
                │
                │   match{} : groups / models (supports *) / min_tokens / max_tokens
                │   expr    : boolean over  w, t, tokens, group, model
                │
                │   The probe supplying w and t is called ONLY when some enabled
                │   rule's expr references w or t — once per request. If it fails,
                │   w and t are simply 0.
                │
                │  ▼ matched rule  (none → fallback, see below)
                │
 ═══════════════▼═══ (2) CANDIDATES ═════════════════════════════════════════
                │
                │   effective model = rule.target_model if set, else the
                │                     model the client requested
                │
     ┌─────────────────────────────────────────┬──────────────────────────────────────┐
     │ rule NAMED its targets?                 │ no targets named, or no rule matched │
     │ (target_channel_ids, else target_group) │                                      │
     │                                         │                                      │
     │ YES → those channels ARE the answer.    │ → any enabled channel that SERVES    │
     │      The model name does NOT filter     │   the effective model (its models    │
     │      them. Channel routing beats        │   list, or a model_mapping key)      │
     │      model routing.                     │                                      │
     └─────────────────────────────────────────┴──────────────────────────────────────┘
                │
                │   No candidate? The request FAILS, naming the rule. It does
                │   not fall through to a lower-priority rule — that would turn
                │   quality tiering into a silent downgrade.
                │
 ═══════════════▼═══ (3) LOAD BALANCE ═══════════════════════════════════════
                │
                │   Bucket candidates by channel.priority DESCENDING (opposite
                │   direction to rule priority), take the top bucket, and pick
                │   one inside it by weighted random (weight <= 0 counts as 1).
                │
                │  ▼ primary pick, then the rest in order as failover candidates
                │
 ═══════════════▼═══ (4) CALL + FAILOVER ════════════════════════════════════

     for each candidate, up to RouterMaxRetries (default 3):

        network error ................. retry the next candidate
        upstream 5xx (retryable) ...... retry the next candidate
        upstream 4xx (not retryable) .. surface to the client, stop
        no price / no candidate ....... configuration error, stop
```

### Which model actually gets called

Three layers, each answering a different question. The order matters:

```
  rule      "which model should serve this?"    target_model, else the requested name
    │
    ▼
  channel   "what is that model called HERE?"   model_mapping
    │
    ▼
  adapter   "any provider prefix needed?"       Bedrock cross-region us./eu./apac.
    │
    ▼
  the id actually sent upstream
```

The middle layer is not optional: the same Opus is `claude-opus-4-8` on an
Anthropic-compatible gateway and `anthropic.claude-opus-4-8` on Bedrock. Because the
channel translates, one rule can target both.

That resolved id is then what the rest of the request uses — price lookup, the
prompt-cache minimum-length threshold (4096 tokens for Haiku, 1024 otherwise), the
Bedrock inference-profile prefix, and `request_logs.upstream_model`. The log's `model`
column keeps the client's original name, so the two stay distinguishable.

### Worked example: cheap by default, expensive when it matters

One channel serving three tiers. Rules ascending, strictest first:

| priority | `expr` | `target_model` | serves |
|---:|---|---|---|
| 0 | `w == 1 && t > 500` | opus | writes / tool-calls with long output |
| 1 | `w == 1 or t > 150` | sonnet | anything write-ish or mid-length |
| 2 | *(empty — catch-all)* | haiku | everything else |

Write rule 1's condition as `w == 1 || t > 150`; it is spelled out here only because a
table cell would otherwise need the pipes escaped.

A request for `claude-opus-4-8` that falls through to rule 2 is served by **haiku**: the
rule names the channel, so the requested name does not disqualify it, and `target_model`
settles what actually runs. Billing follows haiku's price, and the log records
`model=claude-opus-4-8` with `upstream_model=<haiku's id>`.

> **Two priority fields, opposite directions.** Rule priority is ascending — the lower
> number matches first. Channel priority is descending — the higher number is preferred.

To tier on prompt size alone, with no probe and no SageMaker cost, use
`match.min_tokens` / `max_tokens` or an `expr` over `tokens`, and keep `w`/`t` out of
every rule.

---

## End-to-end walkthrough

This is the full happy path, from a fresh deploy to calling the relay with an OpenAI and
an Anthropic client.

### 1. Register the first account (becomes admin)

Open `http://localhost:8080`. On first visit the setup flow detects there are no users
yet and prompts you to register — **the very first account created becomes an `admin`**.
(Alternatively, set `ADMIN_USERNAME`/`ADMIN_PASSWORD` in `.env` before first start to
seed an admin automatically.)

Via API:

```bash
curl -s http://localhost:8080/api/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin12345","display_name":"Admin"}'
# → 201 { "token": "<jwt>", "user": { "role": "admin", ... } }
```

### 2. Log in

The UI logs you in automatically after registration. To log in again later, or via API:

```bash
TOKEN=$(curl -s http://localhost:8080/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"admin12345"}' | jq -r .token)
```

### 3. Configure a channel (upstream)

In the UI go to **Channels → New**. Pick `type=openai`, set the upstream `base_url`
(e.g. `https://api.openai.com`), paste the upstream API key, and set a `group`
(default `default`). The key is stored AES-256-GCM encrypted and only ever shown masked.

Via API:

```bash
curl -s http://localhost:8080/api/channels \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name":"openai-main",
        "type":"openai",
        "base_url":"https://api.openai.com",
        "key":"sk-REPLACE-WITH-UPSTREAM-KEY",
        "group":"default",
        "models":["gpt-4o-mini"],
        "status":"enabled"
      }'
```

### 4. Fetch models

Click **Auto-fetch models** on the channel (or `POST /api/channels/:id/fetch-models`).
For OpenAI channels this calls the upstream `GET {base_url}/v1/models` and stores the
returned model ids (cached in Redis for 10 minutes). For Bedrock channels a built-in
model id list is offered. You can also edit the model list by hand.

```bash
curl -s -X POST http://localhost:8080/api/channels/1/fetch-models \
  -H "Authorization: Bearer $TOKEN"
```

### 5. (Optional) Create a routing rule

In **Routing Rules → New** you can express *"requests matching these groups / models /
token-size land on these channels"*. With a single channel you can skip this — when no
rule matches, the engine falls back to any enabled channel that can serve the requested
model.

```bash
curl -s http://localhost:8080/api/rules \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
        "name":"default-route",
        "enabled":true,
        "priority":0,
        "match":{"groups":["default"]},
        "target_group":"default"
      }'
```

#### Model-tier routing (`target_model`)

A rule can also declare **which upstream model serves the request**, not just which
channel. That is what lets one channel carry several quality tiers — expensive model for
hard turns, cheap model for simple ones — instead of needing one channel per model.

Rules are evaluated by ascending `priority` and the first match wins, so order them
strictest-first:

```bash
# hard turns (write/tool-call AND long output) → the expensive model
curl -s http://localhost:8080/api/rules -X POST \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"hard-tier","enabled":true,"priority":0,
       "match":{"groups":["default"]},
       "expr":"w == 1 && t > 500",
       "target_channel_ids":[1],
       "target_model":"anthropic.claude-opus-4-8"}'

# everything else → the cheap model (no expr = always matches, so keep it last)
curl -s http://localhost:8080/api/rules -X POST \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"easy-tier","enabled":true,"priority":9,
       "match":{"groups":["default"]},
       "target_channel_ids":[1],
       "target_model":"anthropic.claude-haiku-4-5-20251001-v1:0"}'
```

How it behaves:

- **Leave `target_model` empty** and the behaviour is exactly as before — the client's
  requested model is used.
- When set, it replaces the requested model for **both** upstream resolution *and*
  candidate-channel filtering. The filtering half is the useful one: the channel no longer
  has to declare whatever model name the client happens to send, so N models stop
  requiring N channels.
- The channel's `model_mapping` still applies **on top** of it — the rule says *which
  model*, the channel says *what that model is called here* (e.g. the same Opus is
  `claude-opus-4-8` on a gateway and `anthropic.claude-opus-4-8` on Bedrock).
- Billing, the prompt-cache threshold and `request_logs.upstream_model` all follow the
  override, so cost is charged against the model that actually ran.
- If nothing can serve the `target_model`, the request **fails** naming the rule and the
  model — it does *not* silently fall through to a cheaper tier, which would defeat the
  point of quality tiering. Configure a price for every (channel, target_model) pair.

`w` and `t` come from the optional routing probe (see the probe settings page); rules that
never reference them never invoke it. To tier on prompt size alone with no probe, use
`match.min_tokens` / `max_tokens` or an `expr` over `tokens` instead.

### 6. Generate an API key

In **API Keys → New**. The full `sk-...` key is shown **once** — copy it now; afterwards
only a masked form is stored/displayed.

```bash
SK=$(curl -s http://localhost:8080/api/tokens \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"my-key","group":"default","quota":-1}' | jq -r .key)
echo "$SK"   # sk-...   (quota -1 = unlimited)
```

### 6b. (Optional) Try a channel in the Playground

Before wiring up a downstream client you can talk to any channel directly from the admin UI.
Open **Channels → Playground** for a **ChatGPT-style chat window**: a multi-turn conversation
with Markdown + code-block rendering, streaming with a typing cursor, per-message **copy** and
**regenerate**, an optional **system prompt**, multi-line input (Enter sends / Shift+Enter
newlines), and image upload/paste for multimodal models. It calls `POST /api/channels/:id/test-chat`
against the one chosen channel — it does **not** consume quota and is **not** keyed by an API
key. Each test-chat is still written to the request log tagged `is_test=true` (with `token_id`
NULL) so it is auditable, yet the **Dashboard** summary/timeseries default to production-only
traffic so test runs never skew your metrics. In the Dashboard **logs** table use the
**type** filter (All / Production / Test) to view test-chat rows; upstream error text is
captured in the log's `error_message`.

### 7. Call the relay

The gateway accepts both inbound formats and routes them to the matching upstream.

**OpenAI client** (`/v1/chat/completions`):

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-...")  # your sk- key
resp = client.chat.completions.create(
    model="gpt-4o-mini",
    messages=[{"role": "user", "content": "Hello!"}],
)
print(resp.choices[0].message.content)
```

or with curl:

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $SK" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello!"}]}'
```

**Anthropic client** (`/v1/messages`):

```python
import anthropic
client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="sk-...")
msg = client.messages.create(
    model="claude-3-5-sonnet-20240620",
    max_tokens=256,
    messages=[{"role": "user", "content": "Hello!"}],
)
print(msg.content[0].text)
```

or with curl:

```bash
curl -s http://localhost:8080/v1/messages \
  -H "x-api-key: $SK" -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \
  -d '{"model":"claude-3-5-sonnet-20240620","max_tokens":256,
       "messages":[{"role":"user","content":"Hello!"}]}'
```

Both endpoints support `"stream": true` (SSE). Every call is metered against the key's
and the user's quota and written to the request log; view it under **Dashboard**.

> **Quota note:** relay calls are gated by `min(token quota, user quota)`. A brand-new
> user starts with the `DefaultUserQuota` option (which ships at `0`), so calls return
> HTTP `402 insufficient_quota` until the user is given budget. As admin, raise it under
> **Users** in the UI (or `PUT /api/users/:id` with `{"quota": 1000000}`), and/or bump the
> `DefaultUserQuota` system option. A token `quota` of `-1` means "unlimited for the token"
> but the user-level quota still applies.

---

## Configuration reference

All variables are documented in [`.env.example`](./.env.example). Key ones:

| Variable                          | Required | Default        | Purpose                                            |
|-----------------------------------|----------|----------------|----------------------------------------------------|
| `HTTP_PORT`                       | no       | `8080`         | Host port the UI/API are published on              |
| `POSTGRES_USER/PASSWORD/DB`       | no       | `postgres/.../agent_router` | Database credentials (shared by db + backend) |
| `POSTGRES_SSLMODE`                | no       | `disable`      | Postgres SSL mode                                  |
| `REDIS_PASSWORD` / `REDIS_DB`     | no       | empty / `0`    | Redis auth / db index                              |
| `JWT_SECRET`                      | **yes**  | —              | HS256 secret signing admin JWTs                    |
| `SECRET_KEY`                      | **yes**  | —              | Encrypts stored channel keys (AES-256-GCM)         |
| `GIN_MODE`                        | no       | `release`      | `release` / `debug` / `test`                       |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | no     | empty          | If both set, seed an admin on first start          |
| `BACKEND_ORIGIN`                  | no       | `backend:3000` | *frontend container only, set per-topology rather than via `.env`* — where nginx proxies `/api` and `/v1`. `docker-compose.yml` pins it to `backend:3000`; the ECS template sets `127.0.0.1:3000`, since `awsvpc` gives both containers one network namespace and no service DNS. |

> `SECRET_KEY` may be any length: the server derives a 32-byte AES-256 key from it with
> SHA-256. Use a long random value in production. Changing it later makes previously
> stored channel keys undecryptable, so set it once before configuring channels.

Inside the compose network the backend reaches the database at `postgres:5432` and Redis
at `redis:6379` — these hostnames are the compose service names and are wired in
`docker-compose.yml`; you do not set `DB_DSN`/`REDIS_ADDR` yourself.

---

## Local development (without Docker)

You still need PostgreSQL and Redis reachable. The quickest way is to run just those two
via compose and develop the apps natively:

```bash
docker compose up -d postgres redis
```

### Backend (`/server`)

```bash
cd server
export PORT=3000
export POSTGRES_HOST=localhost POSTGRES_PORT=5432 \
       POSTGRES_USER=postgres POSTGRES_PASSWORD=postgres POSTGRES_DB=agent_router
export REDIS_ADDR=localhost:6379
export JWT_SECRET=dev-jwt-secret
export SECRET_KEY=dev-secret-key
export GIN_MODE=debug
go run .          # serves http://localhost:3000  (GET /api/ping → pong)
```

Run the test suite:

```bash
cd server && go build ./... && go test ./...
```

### Frontend (`/web`)

```bash
cd web
npm install
npm run dev       # Vite dev server (default http://localhost:5173)
```

`vite.config.js` proxies `/api` and `/v1` to the backend (`http://localhost:3000`) during
development, so the SPA talks to your local `go run` server. Build the production bundle
with `npm run build` (outputs `web/dist`, which the nginx container serves in production).

---

## Project layout

```
/server            Go + Gin backend (Dockerfile = multi-stage build → alpine)
  main.go          entrypoint: load config → connect DB/Redis → migrate → serve
  config/          env parsing (PORT, POSTGRES_*/DB_DSN, REDIS_ADDR, JWT_SECRET, SECRET_KEY)
  internal/        model, db, middleware, service, router engine, adapters, relay, controllers
/web               React + Vite + Semi Design SPA (Dockerfile = build → nginx)
  nginx.conf.template  serves dist + reverse-proxies /api and /v1 to $BACKEND_ORIGIN
                       (backend:3000 under compose, 127.0.0.1:3000 under ECS awsvpc,
                        where both containers share one network namespace)
docker-compose.yml four services: postgres / redis / backend / frontend
/deploy            CloudFormation: cloudformation.yml (single EC2) and
                   cloudformation-ecs.yml (ECS on EC2 + RDS + ElastiCache); see
                   deploy/README.md
.env.example       all configuration variables with defaults
```

For the full technical design see the project's Tech Design document (Agent Router
Platform MVP) — §2 layout, §10 deployment, §11 non-functional/security.
