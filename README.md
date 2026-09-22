# AI Gateway

[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Tests](https://img.shields.io/badge/Tests-74%20Passing-brightgreen.svg)]()
[![Binary Size](https://img.shields.io/badge/Binary-8.4MB%20(Static)-success.svg)]()

> **"AI proposes. Deterministic systems enforce. Boring, reliable infrastructure."**

**AI Gateway** is a high-performance, production-ready control plane positioned between your applications and Large Language Model (LLM) providers (OpenAI, Anthropic, Google Vertex AI, Azure OpenAI, and self-hosted vLLM/Triton clusters).

Designed as a drop-in replacement for OpenAI API endpoints, it delivers unified multi-provider routing, deterministic data security (DLP/PII masking), token quota enforcement, circuit-breaking resilience, and cryptographic auditability without sacrificing streaming latency ($p_{99} < 15\text{ms}$ gateway overhead).

---

## Why AI Gateway?

Directly connecting frontend apps, backend microservices, and AI agents to LLM APIs creates operational fragility, security exposure, and unchecked cloud spend. AI Gateway provides a unified, policy-enforcing perimeter:

- **Drop-in OpenAI Compatibility**: Works natively with the official OpenAI Python/Node SDKs, LangChain, LlamaIndex, and `curl` by simply pointing `base_url` to the gateway.
- **Deterministic Data Loss Prevention (DLP)**: Real-time linear-time RE2 regex and Shannon entropy scanners intercept credit cards (Luhn validated), SSNs, AWS keys, and private keys. Configurable `BLOCK` and `MASK` actions prevent data leaks.
- **Fixed Lookahead Buffer for Streaming SSE**: Zero-copy lookahead buffer ($W=128\text{B}, L=64\text{B}$) intercepts sensitive patterns split across multiple streaming chunks without degrading Time-To-First-Token (TTFT).
- **Two-Phase Token Reservation & Rate Limiting**: Speculatively reserves tokens before dispatch based on prompt length, reconciling exact usage on completion. Enforces tenant Requests-Per-Minute (RPM), Tokens-Per-Minute (TPM), and concurrency semaphores.
- **SRE Resilience & Circuit Breaking**: Fast-fails broken upstreams using a sliding-window circuit breaker (`Closed`, `Open`, `Half-Open`) with exponential cooldown doubling and automatic failover across priority tiers (e.g. `gpt-4o` $\to$ `claude-3-5-sonnet` $\to$ on-prem `llama-3.3-70b`).
- **Instant Zombie Socket Teardown**: When a user or client disconnects mid-stream, the gateway immediately terminates the upstream HTTP connection, ending billable token generation.
- **Cryptographic Audit Ledger**: Records request/response SHA-256 digests in an append-only, tamper-evident hash-chained ledger ($H_i = \text{SHA-256}(H_{i-1} \parallel \dots)$).
- **Native Observability**: Exposes Prometheus metrics (`/metrics`) isolating gateway overhead from upstream model duration, alongside W3C `traceparent` distributed tracing.

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

### 1. Run with Docker (Recommended)

```bash
# Build the minimal production container
docker build -t ai-gateway:latest .

# Run the gateway with default configuration
docker run -d -p 8080:8080 \
  -e OPENAI_API_KEY="your-upstream-openai-key" \
  --name ai-gateway ai-gateway:latest
```

### 2. Run Native Binary

```bash
# Build static binary
go build -ldflags="-s -w" -o bin/ai-gateway ./cmd/gateway

# Launch with reference configuration
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

---

## Production Capabilities Built Into the Core

The engine is structured as an explicit, high-throughput linear stage pipeline ([ADR-002](file:///root/ai-gateway/docs/adr/ADR-002-proxy-pipeline-architecture.md)):

### 1. Ingress & Multi-Tenant Authentication
- Authenticates callers using constant-time comparison ([`crypto/subtle`](file:///root/ai-gateway/internal/auth/auth.go)) to eliminate timing attacks.
- Scopes tenants to authorized model routes (`allowed_routes`) and tiers.
- Serves `/healthz/liveness`, `/healthz/readiness`, and tenant-filtered `/v1/models`.

### 2. SRE Resilience & Health Engine
- **Circuit Breaker State Machine**: Monitors error rates over 60s sliding windows. Trips at $\ge 50\%$ failure rate ($N_{\min} = 20$). Enforces 30s cooldown before sending canary probes. Consecutive probe failures double the cooldown up to 300s.
- **Priority Fallback Cascades**: Seamlessly diverts traffic across tiers (`Tier 0` $\to$ `Tier 1` $\to$ `Tier 2`) on upstream 502/503/504 errors or network timeouts.
- **Adaptive Full Jitter Retries**: Implements decorrelated jitter backoff and parses `Retry-After` headers.
- **Readiness Integration**: `/healthz/readiness` dynamically reflects upstream health and circuit breaker states.

### 3. Deterministic Policy & Rate Limiting
- **Two-Phase Token Reservation (TPM)**: Pre-dispatch token estimation prevents quota overages before hitting upstreams; post-dispatch reconciliation refunds unused tokens.
- **Deterministic DLP**: Scans prompts and completions with linear RE2 regex patterns (Credit Cards with Luhn validation, SSNs, AWS Secret Keys, PEM private keys) and Shannon entropy ($H(X) \ge 4.5$).
- **Streaming Lookahead Buffer**: $128\text{B}$ buffer with $64\text{B}$ overlap catches multi-chunk sensitive patterns in SSE streams.

### 4. Observability & Cryptographic Audit
- **Prometheus Metrics**: Dedicated `/metrics` endpoint exporting:
  - `ai_gateway_overhead_duration_seconds`: Fine-grained proxy latency histogram ($p_{50} < 2\text{ms}, p_{99} < 15\text{ms}$).
  - `ai_gateway_requests_total`, `ai_gateway_tokens_total`, `ai_gateway_circuit_breaker_state`, `ai_gateway_ratelimit_rejections_total`.
- **W3C Distributed Tracing**: Generates and propagates `traceparent` headers across downstream responses and upstream dispatches.
- **Tamper-Evident Audit Ledger**: Hashes all transactions into an unbroken SHA-256 chain ($H_0 = \text{"0"} \times 64$). `VerifyChain` validates log integrity and immediately pinpoints any altered or missing record.

---

## Architectural Documentation & ADRs

The system is fully documented with formal specifications and design records:

| Document | Description |
| :--- | :--- |
| **[DESIGN.md](file:///root/ai-gateway/DESIGN.md)** | Master technical design document synthesizing the complete architecture and verification evidence. |
| **[ARCHITECTURE.md](file:///root/ai-gateway/docs/ARCHITECTURE.md)** | Core system topology, 8-stage pipeline, Go interfaces, and canonical domain model definitions. |
| **[SECURITY-BOUNDARIES.md](file:///root/ai-gateway/docs/SECURITY-BOUNDARIES.md)** | Zero trust boundaries, HKDF tenant isolation, KMS key vaulting, and linear RE2 DLP specifications. |
| **[THREAT-MODEL.md](file:///root/ai-gateway/docs/THREAT-MODEL.md)** | Formal STRIDE evaluation (19 threat vectors), prompt injection defenses, and denial-of-wallet mitigations. |
| **[FAILURE-MODES.md](file:///root/ai-gateway/docs/FAILURE-MODES.md)** | Comprehensive FMEA matrix, circuit breaker state machine math, and Full Jitter backoff algorithms. |
| **[SLO.md](file:///root/ai-gateway/docs/SLO.md)** | Gateway overhead latency budgets, 99.95% availability SLO, multi-window burn rate alerts, and metric catalog. |
| **[OPERATIONS.md](file:///root/ai-gateway/docs/OPERATIONS.md)** | Production systemd/Kubernetes configurations, zero-downtime rolling deploys, and incident runbooks. |
| **[ADR-001](file:///root/ai-gateway/docs/adr/ADR-001-language-and-runtime-selection.md)** | Runtime Decision: Go 1.23+ with standard library `net/http` vs Rust, Python, and Envoy. |
| **[ADR-002](file:///root/ai-gateway/docs/adr/ADR-002-proxy-pipeline-architecture.md)** | Pipeline Pattern: Explicit linear stage machine with zero-copy SSE streaming vs nested middleware. |
| **[ADR-003](file:///root/ai-gateway/docs/adr/ADR-003-state-and-rate-limiting.md)** | State Management: Sliding-window quotas, two-phase reservation, and fail-secure vs fail-soft defaults. |
| **[ADR-004](file:///root/ai-gateway/docs/adr/ADR-004-streaming-policy-enforcement.md)** | Streaming DLP: Fixed sliding lookahead buffer ($W=128\text{B}, L=64\text{B}$) vs sentence-boundary stalls. |

---

## Configuration Reference

The gateway is configured via a single YAML file supporting environment variable substitution (`${VAR:-default}`). See [`config.example.yaml`](file:///root/ai-gateway/config.example.yaml) for a complete template:

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

The entire system is thoroughly tested with unit, chaos, and integration tests simulating live providers:

```bash
# Run the full test suite with race detector
go test -v -count=1 ./...
```

**74 total tests pass with 0 failures** across unit, resilience, rate limiting, DLP, streaming lookahead, Prometheus metrics, and cryptographic ledger verification.
