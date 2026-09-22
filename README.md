# AI Gateway

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Tests](https://img.shields.io/badge/Tests-74%20Passing-brightgreen.svg)]()
[![Binary Size](https://img.shields.io/badge/Binary-8.4MB%20(Static)-success.svg)]()

**AI Gateway** is a high-performance reverse proxy and control plane positioned between your applications and Large Language Model (LLM) providers (OpenAI, Anthropic, Google Vertex AI, Azure OpenAI, NVIDIA NIM, and self-hosted vLLM/Triton clusters).

Designed as a drop-in replacement for OpenAI API endpoints, it delivers unified multi-provider routing, data loss prevention (DLP/PII masking), token quota enforcement, circuit-breaking resilience, and cryptographic auditability without sacrificing streaming latency ($p_{99} < 15\text{ms}$ gateway overhead).

---

## The 5 Real-World Problems It Solves

Connecting applications directly to third-party LLM APIs creates operational fragility, security exposure, and unchecked cloud spend. AI Gateway provides a unified control plane solving five core operational challenges:

### 1. Instant Failover During Provider Outages (Reliability & SRE)
- **Without Gateway**: When a provider experiences a 503 outage or 429 rate limit spike, your application crashes and your users receive error screens.
- **With Gateway**: The gateway's sliding-window circuit breaker detects failures in milliseconds and automatically diverts requests to secondary fallback tiers (`gpt-4o` $\to$ `claude-3-5-sonnet` $\to$ self-hosted `llama-3.3-70b`). **Users experience uninterrupted uptime.**

### 2. Stopping PII Leaks & Credential Sprawl (Security & DLP)
- **Without Gateway**: Master API keys are distributed across dozens of microservice configuration files. When an end-user pastes an API key, database URL, credit card, or Social Security Number into a prompt, it is transmitted uninspected to external cloud APIs.
- **With Gateway**: Master provider credentials remain vaulted in the gateway; callers receive restricted, tenant-scoped API keys. The gateway's DLP engine scans prompts in linear time ($O(n)$ RE2), automatically **masking or blocking** credit cards (with Luhn checksum validation), SSNs, private keys, and cloud credentials before they leave your network perimeter.

### 3. Preventing "Denial of Wallet" & Runaway Bills (Cost Control)
- **Without Gateway**: A recursive agent enters an infinite loop, or a user runs a batch script that burns through 50 million tokens overnight, generating massive unexpected cloud invoices.
- **With Gateway**: The gateway enforces multi-tenant Requests-Per-Minute (RPM) and Tokens-Per-Minute (TPM) budgets using two-phase speculative token reservation. It estimates prompt tokens *before* dispatch and rejects excess traffic with HTTP 429 *before* spending upstream inference budget.

### 4. Ending "Zombie Token" Spend on Client Disconnects (Efficiency)
- **Without Gateway**: When a user closes their browser tab or cancels a request 2 seconds into a 3,000-token generation, upstream providers continue generating and billing for tokens that are never read.
- **With Gateway**: The moment the downstream client disconnects, the gateway cancels the request context and **immediately terminates the upstream socket**, halting billable token generation instantly.

### 5. Multi-Tenant Cost Attribution & Compliance Auditing (Observability)
- **Without Gateway**: Organizations cannot attribute AI spending accurately across teams or business units because all applications share common API credentials.
- **With Gateway**: Every request and token is attributed to a specific tenant ID or cost center. Native Prometheus metrics (`/metrics`) track latency and token spend per team in real time, while an append-only **cryptographic SHA-256 audit ledger** provides a tamper-evident compliance record.

---

## Architecture Overview

```mermaid
flowchart TD
    subgraph Clients["Applications & Agents"]
        AppA["Backend Microservices"]
        AppB["Web & Mobile Apps"]
        AppC["Autonomous Agents"]
    end

    subgraph Gateway["AI Gateway Engine (Go Process)"]
        Ingress["Ingress Listener (HTTP/1.1, HTTP/2, SSE)"]
        Tracing["W3C Trace Context (traceparent)"]
        Auth["Stage 1: Auth & Constant-Time Key Verification"]
        Quota["Stage 2: RPM Sliding Window & 2-Phase TPM Quota"]
        Router["Stage 3: Route Resolution & Circuit Breaker"]
        DLPIn["Stage 4: Inbound DLP (RE2 & Shannon Entropy)"]
        Dispatch["Stage 5: Upstream Dispatcher & Client Pool"]
        StreamEngine["Stage 6: Zero-Copy SSE Stream Lookahead Buffer"]
        DLPOut["Stage 7: Outbound Redaction & Validation"]
        Audit["Stage 8: Cryptographic Audit Ledger & Prometheus"]
    end

    subgraph Providers["Model Providers"]
        OAI["OpenAI (Tier 0 Primary)"]
        Anthropic["Anthropic (Tier 1 Fallback)"]
        Local["Local vLLM / Triton (Tier 2 Emergency)"]
    end

    Clients -->|OpenAI Wire Protocol| Ingress
    Ingress --> Tracing
    Tracing --> Auth
    Auth --> Quota
    Quota --> Router
    Router --> DLPIn
    DLPIn --> Dispatch

    Dispatch -->|SSE Chunks| StreamEngine
    Dispatch -->|JSON Buffer| DLPOut
    StreamEngine --> Ingress
    DLPOut --> Ingress

    Dispatch -->|HTTPS| OAI
    Dispatch -->|HTTPS| Anthropic
    Dispatch -->|HTTP/2| Local

    Ingress -.-> Audit
```

---

## Quickstart

### 1. Run with Docker

```bash
# Build the production container
docker build -t ai-gateway:latest .

# Run the gateway with default configuration
docker run -d -p 8080:8080 \
  -e OPENAI_API_KEY="your-upstream-openai-key" \
  --name ai-gateway ai-gateway:latest
```

### 2. Run Native Binary

Requires Go 1.23+:

```bash
# Build static binary
go build -ldflags="-s -w" -o bin/ai-gateway ./cmd/gateway

# Launch with configuration file
./bin/ai-gateway -config config.example.yaml
```

The gateway listens on `0.0.0.0:8080`.

---

## Client Usage Examples

### Using `curl` (Chat Completion)

```bash
curl -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-live-analytics-key-4a8f9c" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "system", "content": "You are a helpful assistant."},
      {"role": "user", "content": "Explain circuit breakers in 2 sentences."}
    ],
    "temperature": 0.7
  }'
```

### Streaming Responses (Server-Sent Events)

```bash
curl -N -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-gw-live-analytics-key-4a8f9c" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "prod-fast",
    "messages": [{"role": "user", "content": "Stream the numbers 1 to 5"}],
    "stream": true
  }'
```

### Using Official OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="sk-gw-live-analytics-key-4a8f9c"
)

response = client.chat.completions.create(
    model="gpt-4o",
    messages=[{"role": "user", "content": "Hello via AI Gateway!"}],
    stream=True
)

for chunk in response:
    content = chunk.choices[0].delta.content
    if content:
        print(content, end="", flush=True)
```

### Using LangChain

```python
from langchain_openai import ChatOpenAI

llm = ChatOpenAI(
    base_url="http://localhost:8080/v1",
    api_key="sk-gw-live-analytics-key-4a8f9c",
    model="gpt-4o"
)

response = llm.invoke("Hello from LangChain through AI Gateway")
print(response.content)
```

### Using Node.js / TypeScript

```typescript
import OpenAI from "openai";

const client = new OpenAI({
  baseURL: "http://localhost:8080/v1",
  apiKey: "sk-gw-live-analytics-key-4a8f9c",
});

const completion = await client.chat.completions.create({
  model: "gpt-4o",
  messages: [{ role: "user", content: "Hello world" }],
});
```

---

## Core Capabilities

### 1. Ingress & Multi-Tenant Authentication
- Authenticates callers using constant-time comparison to eliminate timing attacks.
- Scopes tenants to authorized model routes (`allowed_routes`), rate limits, and priority tiers.
- Serves `/healthz/liveness`, `/healthz/readiness`, and tenant-filtered `/v1/models`.

### 2. SRE Resilience & Health Engine
- **Circuit Breaker State Machine**: Monitors error rates over 60s sliding windows. Trips at $\ge 50\%$ failure rate ($N_{\min} = 20$). Enforces 30s cooldown before sending canary probes. Consecutive probe failures double the cooldown up to 300s.
- **Priority Fallback Cascades**: Seamlessly diverts traffic across tiers (`Tier 0` $\to$ `Tier 1` $\to$ `Tier 2`) on upstream 502/503/504 errors or network timeouts.
- **Adaptive Full Jitter Retries**: Implements decorrelated jitter backoff and parses `Retry-After` headers.
- **Readiness Integration**: `/healthz/readiness` dynamically reflects upstream health and circuit breaker states.

### 3. Policy Enforcement & Rate Limiting
- **Two-Phase Token Reservation (TPM)**: Pre-dispatch token estimation prevents quota overages before hitting upstreams; post-dispatch reconciliation refunds unused tokens.
- **Deterministic DLP**: Scans prompts and completions with linear RE2 regex patterns (Credit Cards with Luhn validation, SSNs, AWS Secret Keys, PEM private keys) and Shannon entropy ($H(X) \ge 4.5$).
- **Streaming Lookahead Buffer**: $128\text{B}$ buffer with $64\text{B}$ overlap catches multi-chunk sensitive patterns in SSE streams.

### 4. Observability & Cryptographic Audit
- **Prometheus Metrics**: Dedicated `/metrics` endpoint exporting:
  - `ai_gateway_overhead_duration_seconds`: Fine-grained proxy latency histogram ($p_{50} < 2\text{ms}, p_{99} < 15\text{ms}$).
  - `ai_gateway_requests_total`, `ai_gateway_tokens_total`, `ai_gateway_circuit_breaker_state`, `ai_gateway_ratelimit_rejections_total`.
- **W3C Distributed Tracing**: Generates and propagates `traceparent` headers across downstream responses and upstream dispatches.
- **Tamper-Evident Audit Ledger**: Hashes all transactions into an unbroken SHA-256 chain ($H_0 = \text{"0"} \times 64$) with built-in chain verification.

---

## Configuration Reference

The gateway is configured via a single YAML file supporting environment variable substitution (`${VAR:-default}`). See [`config.example.yaml`](config.example.yaml) for a complete template:

```yaml
server:
  host: "0.0.0.0"
  port: 8080
  read_timeout_seconds: 15
  write_timeout_seconds: 60
  max_body_bytes: 16777216

upstreams:
  - id: "openai-primary"
    provider: "openai"
    endpoint_url: "https://api.openai.com"
    api_key: "${OPENAI_API_KEY}"
    timeout_seconds: 30

routes:
  - alias: "gpt-4o"
    tiers:
      - priority: 0
        targets:
          - upstream_id: "openai-primary"
            model: "gpt-4o"

tenants:
  - id: "tenant-analytics"
    name: "Analytics & Production Engineering"
    tier: "production"
    api_keys:
      - "sk-gw-live-analytics-key-4a8f9c"
    allowed_routes:
      - "gpt-4o"
    rate_limits:
      requests_per_minute: 600
      tokens_per_minute: 1000000
      max_concurrent: 50
    policy_bindings:
      - "dlp-strict"

policies:
  - id: "dlp-strict"
    name: "Strict PII & Credentials Filter"
    action: "BLOCK"
    patterns:
      - credit_card
      - ssn
      - aws_key
      - private_key
```

---

## Verification & Testing

The test suite exercises unit, resilience, rate limiting, DLP, streaming lookahead, Prometheus metrics, and cryptographic ledger verification:

```bash
# Run the full test suite
go test -v -count=1 ./...
```

**74 total tests pass with 0 failures.**

---

## License

MIT License. See [LICENSE](LICENSE) for details.
