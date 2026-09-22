# ADR-004: Streaming Policy Enforcement and Low-Latency DLP

## Status
Accepted

## Date
2026-09-22

## Related Architecture and Threat Modeling
- Security Boundaries: [SECURITY-BOUNDARIES.md](file:///root/ai-gateway/docs/SECURITY-BOUNDARIES.md)
- Threat Model: [THREAT-MODEL.md](file:///root/ai-gateway/docs/THREAT-MODEL.md)
- Foundational Philosophy: [00-PROJECT-PHILOSOPHY.md](file:///root/company-project-specs/00-PROJECT-PHILOSOPHY.md)

## Context
Interactive AI applications (chatbots, coding assistants, agent workflows) rely on streaming HTTP responses via Server-Sent Events (SSE, `text/event-stream`). Upstream model providers emit output incrementally as discrete tokens, typically ranging from 15 to 80 tokens per second.

From a user experience perspective, **Time-To-First-Token (TTFT)** is the primary operational metric. A responsive system demands a TTFT under 250 milliseconds.

However, from an enterprise security and compliance perspective, model outputs must be scanned for:
1. Personally Identifiable Information (PII), such as Social Security Numbers and national identifiers.
2. Payment Card Industry (PCI) data, specifically credit card numbers requiring Luhn check validation.
3. Cryptographic secrets and credentials, including AWS keys, GCP tokens, private SSH/RSA keys, and JWTs.
4. Malicious code patterns and unauthorized tool invocation schemas.

### The Streaming Security Dilemma
Sensitive strings rarely arrive within a single SSE chunk. LLM tokenizers (such as BPE) split words and numbers into arbitrary token fragments based on subword frequency. For example:
- An AWS Access Key ID (`AKIAIOSFODNN7EXAMPLE`, 20 bytes) is frequently emitted across four or five chunks: `["AKIA", "IOSF", "ODNN", "7EXA", "MPLE"]`.
- A credit card number (`4111-2222-3333-4444`, 19 bytes) arrives across multiple chunks: `["4111", "-", "2222", "-", "3333", "-", "4444"]`.

This creates a fundamental conflict between latency and security:
- **Zero Buffering (Naïve Streaming):** If the gateway forwards each SSE chunk to the client immediately upon receipt, it cannot detect patterns spanning chunk boundaries. By the time the trailing chunk arrives, the leading characters have already been transmitted over the wire and displayed in the client's browser or consumed by a downstream agent.
- **Full Stream Buffering:** If the gateway buffers the entire response until generation completes (Time-To-Last-Token, TTLT) before running policy checks, TTFT degrades from 200ms to 10-30 seconds. This destroys the real-time interactive utility of streaming.

---

## Decision Drivers
1. **Deterministic Security Guarantee:** Zero sensitive data leakage across network boundaries under any token chunking distribution.
2. **TTFT Preservation:** Added TTFT latency introduced by the gateway must remain strictly bounded ($< 100\text{ms}$).
3. **Safe Failure Modes:** Fail-closed defaults when policies are violated or scanning deadlines are exceeded.
4. **Memory Scalability:** Bounded memory overhead per connection to support $\ge 10,000$ concurrent streaming connections per gateway node.
5. **Linear Time Execution:** Scanning algorithms must execute in $O(n)$ time relative to payload size, eliminating Regular Expression Denial of Service (ReDoS).

---

## Considered Alternatives

### Alternative 1: Post-Stream Asynchronous Redaction (Optimistic Streaming)
In this model, the gateway streams all SSE chunks to the caller with zero lookahead delay. Simultaneously, it buffers the full completion in background memory. When the stream concludes, a background worker scans the buffer and updates audit logs or fires alerts if sensitive data was present.

- **Pros:** Zero added latency to TTFT.
- **Cons:** Fatal security failure. The caller has already received the unredacted sensitive data in memory and on screen. Violates PCI-DSS, HIPAA, and Zero Trust security principles. Unacceptable for a security gateway.

### Alternative 2: Sentence-Boundary / Punctuation Chunking
In this model, the gateway buffers tokens until it encounters sentence-terminating punctuation (`.`, `!`, `?`, or newline `\n`). The gateway then scans the completed sentence, emits it, and begins buffering the next sentence.

- **Pros:** Preserves natural semantic boundaries for regex evaluation.
- **Cons:** Unbounded and highly variable latency jitter. In code generation, markdown tables, JSON function calling, or run-on prose, sentence delimiters may not appear for hundreds of tokens (causing 5 to 15 seconds of silence). Creates an erratic user experience and unpredictable TTFT.

### Alternative 3: Fixed Sliding Window Lookahead Buffer (Chosen)
In this model, the gateway maintains a small, fixed-capacity rolling byte buffer ($W_{lookahead} = 128\text{ bytes}$) with a designated overlap margin ($L_{overlap} = 64\text{ bytes}$). Tokens are accumulated until the lookahead threshold is met. The gateway scans the window using deterministic linear automata; if clean, the oldest bytes beyond the overlap margin are immediately released to the client.

- **Pros:** Deterministic, bounded latency overhead (80-100ms for the very first chunk, 0ms for subsequent chunks). Mathematically guarantees that any sensitive pattern shorter than $L_{overlap}$ cannot cross the release boundary undetected.
- **Cons:** Slight TTFT latency increase equal to the time required to accumulate 128 bytes from the upstream provider.

---

## Decision
We adopt **Alternative 3: Fixed Sliding Window Lookahead Buffer with Speculative Token Release and Upstream Circuit-Breaker Abort**.

```mermaid
stateDiagram-v2
    [*] --> Accumulating: Upstream SSE Chunk Arrives
    
    Accumulating --> Evaluating: Buffer Length reaches lookahead limit
    Accumulating --> FlushingTail: Upstream Stream Ends with DONE marker
    
    state Evaluating {
        [*] --> RunAhoCorasick: Scan Fixed Signatures
        RunAhoCorasick --> RunLinearRE2: Scan Dynamic Regex
        RunLinearRE2 --> CalcEntropy: Verify High-Entropy Tokens
    }
    
    Evaluating --> ReleasingHead: All Scanners Pass Clean
    Evaluating --> TerminatingViolation: Policy Violation Detected
    
    ReleasingHead --> Accumulating: Emit bytes exceeding overlap margin
    FlushingTail --> TerminatingViolation: Final Scan Detects Secret
    FlushingTail --> Complete: Final Scan Clean
    
    TerminatingViolation --> [*]: Send Error Event and Cancel Upstream
    Complete --> [*]: Forward DONE Event to Caller
```

---

## Detailed Technical Specification

### 1. Window Sizing Mathematics
Let $L_{max}$ be the maximum byte length of any single secret pattern or PII token defined in the gateway's deterministic detection catalog:

| Target Pattern | Maximum Byte Length ($L$) |
| :--- | :--- |
| PCI Credit Card (with separators) | 19 bytes |
| US Social Security Number | 11 bytes |
| AWS Access Key ID | 20 bytes |
| Google Cloud Platform API Key | 39 bytes |
| GitHub Personal Access Token | 40 bytes |
| Private Key PEM Header | 36 bytes |
| Standard High-Entropy Secret Slice | 48 bytes |

The maximum target pattern length is $L_{max} = 48\text{ bytes}$.

To guarantee that no pattern of length $L \le L_{max}$ can cross the release boundary without being completely visible in at least one scanning window, the overlap margin $L_{overlap}$ must satisfy:
$$L_{overlap} \ge L_{max}$$
We set **$L_{overlap} = 64\text{ bytes}$** (providing a 16-byte safety margin).

We set the total lookahead window capacity to:
$$W_{lookahead} = 2 \times L_{overlap} = 128\text{ bytes}$$

### 2. Stream Buffer Execution Algorithm
For each active streaming connection, the gateway maintains an in-memory byte deque `Buf`:

```
+-------------------------------------------------------------+
|                     SLIDING LOOKAHEAD BUFFER                |
|                                                             |
|  [ Released Bytes ] | [ Release Candidate ] | [ Overlap ]   |
|  (Already sent)     | (Eligible to emit)    | (Held back)   |
|                     |                       |               |
|                     |<--------------------->|<------------->|
|                        Release Chunk (64B)    L_overlap (64B)|
|                     |<------------------------------------->|
|                                W_lookahead (128B)            |
+-------------------------------------------------------------+
```

1. **Ingress Chunk Arrival:**
   As raw SSE chunks arrive from the upstream provider, the JSON `data:` field is parsed and the token text content is appended to `Buf`.
2. **Evaluation Trigger:**
   - If `len(Buf) < W_{lookahead}` and stream is active: Continue buffering.
   - If `len(Buf) >= W_{lookahead}`:
     a. Extract scanning slice: `Window = Buf[0 : W_{lookahead}]`.
     b. Execute deterministic scanning pipeline over `Window`.
     c. **If Clean:** 
        - Release slice: `EmitBytes = Buf[0 : (len(Buf) - L_{overlap})]`.
        - Transmit `EmitBytes` as SSE data event to caller.
        - Truncate `Buf`: `Buf = Buf[(len(Buf) - L_{overlap}) :]`.
     d. **If Violation Detected:**
        - Execute violation handler immediately (see Section 3).
3. **Stream Termination ([DONE]):**
   When the upstream provider sends `data: [DONE]`:
   a. Scan the remaining bytes in `Buf[0 : len(Buf)]`.
   b. If clean, emit all remaining bytes to caller, followed by `data: [DONE]`.
   c. If violation detected, execute violation handler; discard remaining buffer.

### 3. Violation Handling and Upstream Circuit Abort
When a DLP rule matches within the sliding window, the gateway takes the following deterministic actions:
1. **Drop Buffer:** All unreleased bytes in `Buf` are immediately zeroed and discarded.
2. **Emit Standardized SSE Error:** The gateway sends a terminal SSE event to the client:
   ```text
   event: policy_violation
   data: {"error":{"type":"policy_violation","code":"OUTPUT_DLP_BLOCKED","rule":"PCI_CREDIT_CARD","message":"Stream terminated due to sensitive data exposure in model completion."}}

   ```
3. **Upstream Cancellation:** The gateway sends an HTTP/2 `RST_STREAM` frame (error code `CANCEL`) to the upstream provider socket. This immediately halts upstream GPU inference and token generation, preventing further token billing.
4. **Audit Recording:** A high-severity audit record is appended to the cryptographic audit chain recording the rule match, token count up to termination, and the SHA-256 hash of the released partial stream.

---

## Tradeoff Analysis and Empirical Impact

### 1. Latency Impact (TTFT Overhead)
- At typical provider generation speeds (30 tokens/sec $\approx 120$ bytes/sec), accumulating the initial $128\text{ bytes}$ takes:
  $$T_{buffer} \approx \frac{128\text{ bytes}}{120\text{ bytes/sec}} \approx 85\text{ milliseconds}$$
- The linear-time scanning engine (vectorized Aho-Corasick + RE2) executes in $< 15\text{ microseconds}$ per 128-byte window on modern x86_64 / ARM64 processors.
- **Net TTFT Impact:** Total TTFT increases by an imperceptible $85\text{ms}$ (e.g. from $180\text{ms}$ to $265\text{ms}$).
- **Inter-Token Latency (ITL):** Once the lookahead buffer is primed, subsequent tokens are emitted in steady state with **zero additional buffering delay**, maintaining smooth UI rendering.

### 2. Memory Footprint at Scale
- Each active streaming connection requires:
  - Ring buffer storage: 128 bytes
  - SSE parser context and framing state: ~384 bytes
  - Total per-stream state: $\approx 512\text{ bytes}$
- For a gateway instance handling 10,000 concurrent streaming connections:
  $$\text{Total Memory Overhead} = 10,000 \times 512\text{ bytes} \approx 5.12\text{ MB}$$
- Memory overhead is negligible and safely bounded.

### 3. Operational Tradeoffs

| Architectural Dimension | Tradeoff Incurred | Justification / Mitigation |
| :--- | :--- | :--- |
| **Stream Truncation** | Client receives a partial, broken completion when a violation occurs. | Acceptable. Leaking credentials or PII to the client is a catastrophic failure; a truncated error message is a safe failure. |
| **Provider Sunk Cost** | Upstream provider bills tokens generated up to the moment of cancellation. | Issuing an immediate HTTP/2 `RST_STREAM` minimizes wasted token spend to within $< 10$ tokens of the violation point. |
| **Redaction Complexity** | In-stream redaction of multi-token words alters token alignment. | Where policy specifies `REDACT` instead of `BLOCK`, the gateway substitutes the matched bytes with fixed tokens (`[REDACTED]`) directly within the lookahead buffer before release. |

---

## Verification and Testing Strategy

The implementation must satisfy the following automated verification criteria before deployment:

1. **Boundary Crossing Test Suite:**
   A test harness splits known PII and secrets (credit cards, AWS keys, JWTs) across chunk boundaries at every possible byte offset (offset 1 through $L-1$). The test asserts 100% detection rate and zero leaked prefix characters.
2. **TTFT Benchmark:**
   Automated load tests measure TTFT across 1,000 streaming requests. The 99th percentile (p99) added latency attributable to the lookahead buffer must remain $\le 110\text{ms}$.
3. **ReDoS Resilience:**
   Synthetic adversarial text streams containing repetitive nested structures are evaluated against the sliding window scanner. Memory and CPU consumption must remain strictly $O(n)$ with zero worker thread lockup.
4. **Upstream RST_STREAM Assertion:**
   Integration tests verify that upon DLP detection, an HTTP/2 `RST_STREAM` frame is transmitted to the upstream mock server within $< 2\text{ms}$.
