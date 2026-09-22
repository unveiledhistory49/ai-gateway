# ADR-001: Language and Runtime Selection for AI Gateway

- **Status**: Accepted
- **Deciders**: Core Architecture Team
- **Date**: 2026-09-22
- **Context File**: `/root/company-project-specs/00-PROJECT-PHILOSOPHY.md`, `/root/company-project-specs/01-ai-gateway.md`

---

## 1. Context and Problem Statement

The enterprise AI Gateway serves as the centralized proxy, policy enforcement layer, and telemetry pipeline between internal client applications and multiple heterogeneous LLM providers (public APIs like OpenAI, Anthropic, and private self-hosted inference clusters like vLLM).

Architecturally, the gateway must handle:
1. High-concurrency, long-duration I/O: Large Language Model requests are fundamentally streaming (Server-Sent Events) and long-lived (ranging from 1 to 120 seconds per generation). The system must sustain thousands of concurrent open connections without memory leaks or thread starvation.
2. Low proxy-induced latency: While upstream LLM Time-To-First-Token (TTFT) ranges from 200ms to 2000ms, the gateway's overhead (routing, token bucket rate limiting, DLP regex scanning, authentication) must remain strictly sub-millisecond (p99 < 5ms).
3. Complex policy & wire-protocol manipulation: Translating between OpenAI REST/SSE wire protocols and provider-specific schemas requires robust JSON parsing, streaming byte-level transformation, and string manipulation.
4. Operational simplicity & maintainability: As dictated by our company philosophy ("Boring, reliable infrastructure over novelty"), the service must deploy as a self-contained, statically compiled artifact with minimal moving parts and zero fragile runtime dependencies.

We must select the core implementation language and runtime environment for the AI Gateway engine.

---

## 2. Decision

We choose **Go (version 1.23+)** utilizing the standard library (`net/http`, `net/http/httputil`, `sync`, `context`) as the foundational runtime for the AI Gateway.

Specifically:
- We leverage standard Go goroutines and network poller (`epoll`/`kqueue`) for non-blocking I/O multiplexing across long-lived SSE connections.
- We utilize Go's built-in `net/http.Transport` connection pooling and `http.Flusher` streaming primitives.
- We produce statically linked binary artifacts (`CGO_ENABLED=0`) deployed inside minimal distroless or `scratch` container images.

---

## 3. Alternatives Considered

We evaluated four candidate runtime environments:

1. **Go 1.23+** (Selected)
2. **Rust** (`tokio` + `hyper` / `axum`)
3. **Python** (`FastAPI` + `uvicorn` / `asyncio`)
4. **Envoy Custom Filter** (C++ or WebAssembly filter on Envoy Proxy)

---

## 4. Deep Technical Comparison

### 4.1 Comparison Matrix

| Criteria | Go 1.23+ (Selected) | Rust (`hyper`/`axum`) | Python (`FastAPI`/`asyncio`) | Envoy Filter (C++/Wasm) |
| :--- | :--- | :--- | :--- | :--- |
| **I/O Concurrency Model** | M:N green threads (goroutines) with netpoller | Explicit async/await state machines on `tokio` | Single-threaded event loop (`asyncio`) | Event-driven multi-threaded event loop |
| **p99 Gateway Latency** | **1.2ms - 3.5ms** | **0.4ms - 1.1ms** | 25ms - 85ms | **0.8ms - 2.0ms** |
| **Memory per 10k Active SSE Streams** | ~180 MB - 250 MB | ~40 MB - 75 MB | ~1.8 GB - 3.2 GB | ~90 MB - 140 MB |
| **GC Overhead / Pause Time** | Concurrent mark/sweep; pauses < 500µs | **Zero (deterministic drop)** | GC pauses + GIL contention under load | Zero (C++) or Wasm linear memory |
| **Streaming Protocol Dexterity** | High (`io.Pipe`, `bufio.Scanner`, `http.Flusher`) | High (`futures::Stream`, `tokio::io`) | Medium (high event loop CPU usage during chunk decoding) | Poor (awkward buffer chaining in C++/Wasm) |
| **Ecosystem & Cloud-Native Tooling** | Unmatched (K8s, Prometheus, OTel, Vault, Docker native) | Growing, but fragmenting async crates | Huge ML ecosystem, weak infrastructure tooling | Highly specialized C++ control plane |
| **Team Maintainability & Velocity** | High: clean syntax, rapid onboarding, strict formatting | Medium-Low: steep borrow-checker curve, async lifetime complexity | High for ML engineers, low for SRE/Platform teams | Very Low: high barrier to entry, slow compile times |
| **Deployment Footprint** | Single static binary (~25MB), `scratch` image | Single static binary (~20MB), `scratch` image | Heavy Docker container (>400MB) with virtualenv | Complex Envoy configuration + shared libraries |

---

### 4.2 Detailed Evaluation of Rejected Alternatives

#### Alternative 1: Rust (`tokio` + `hyper` / `axum`)
*Why it was considered*:
Rust provides exceptional zero-cost abstractions, absolute memory safety without garbage collection, and world-class tail-latency predictability (sub-millisecond p99.9).

*Why it was rejected*:
1. **Developer Ergonomics & Pipeline Evolution**: The AI Gateway requires frequent iterations over dynamic routing logic, multi-provider schema adaptations, and DLP inspection rules. In Rust, handling dynamic async traits (`async fn` in traits / `Pin<Box<dyn Future>>`), complex streaming state transformations, and cross-task context propagation incurs heavy cognitive overhead.
2. **Diminishing Returns on Latency**: An upstream LLM generation request takes between 500ms and 60,000ms. Spending massive engineering bandwidth to shave 0.8ms off a gateway proxy layer when the upstream variance is ±150ms represents a misallocation of systems engineering resources (violating our philosophy of avoiding artificial complexity).
3. **Operational Ecosystem**: While Rust's networking stack is mature, the surrounding ecosystem for cloud infrastructure (OpenTelemetry SDKs, Kubernetes client-go, Vault integration, internal auth middlewares) is native to Go.

#### Alternative 2: Python (`FastAPI` + `uvicorn` / `asyncio`)
*Why it was considered*:
Python is the lingua franca of AI/ML engineering. Many open-source proxies (e.g., LiteLLM) are built on Python.

*Why it was rejected*:
1. **Global Interpreter Lock (GIL) & CPU Bottlenecks**: As SSE streaming throughput rises to thousands of concurrent requests, parsing JSON chunks and executing regex-based DLP filters saturates the Python event loop. A single CPU-bound regular expression scan freezes asynchronous I/O multiplexing across all co-located connections.
2. **High Memory Overhead**: Python's object representation overhead requires ~150KB–300KB per open connection. Under 10,000 concurrent streaming connections, Python consumes gigabytes of RSS memory, making it vulnerable to Out-Of-Memory (OOM) kills under burst loads.
3. **Packaging & Supply Chain Fragility**: Python deployments carry complex virtualenv environments, wheel dependencies, dynamic shared libraries (`libc`), and interpreter startup latency. This contradicts our requirement for clean, deterministic deployments.

#### Alternative 3: Envoy Custom Filter (C++ or WebAssembly)
*Why it was considered*:
Envoy is the industry-standard L7 service mesh proxy. Utilizing Envoy filters would offer out-of-the-box battle-tested connection management, load balancing, and rate limiting.

*Why it was rejected*:
1. **Streaming SSE Manipulation Complexity**: While Envoy excels at static routing and header inspection, parsing dynamic multi-turn conversation payloads, transforming request structures between incompatible provider schemas, and performing token-by-token state tracking in C++ or Wasm is notoriously difficult.
2. **WebAssembly Overhead**: Running Wasm filters inside Envoy introduces memory copying overhead across the host-guest boundary and lacks mature asynchronous HTTP client dispatching for calling auxiliary services (like external Vault or custom auth providers) from within the filter.
3. **Extensibility Barrier**: Modifying and testing C++ Envoy filters requires specialized compilation toolchains, prolonged CI build cycles, and steep operational burdens compared to a self-contained Go microservice.

---

## 5. Quantitative Performance & Runtime Evaluation for Go

### 5.1 Garbage Collection Overhead
A common criticism of Go in high-throughput proxies is garbage collection (GC) latency spikes. We analyzed Go's concurrent mark-and-sweep collector against our operational envelope:
- Go 1.23+ maintains GC pause times strictly under **500 microseconds** (often < 100µs) when allocations are managed properly.
- **Allocation Containment Strategy**:
  - We use `sync.Pool` for reusing 4KB and 8KB streaming byte buffers during SSE chunk decoding.
  - Request contexts and canonical JSON structures are allocated on the stack where escape analysis permits.
  - Zero-allocation string-to-byte casting is employed during regex signature verification.
- **Contextual Impact**: A 0.5ms GC pause is completely imperceptible compared to the 200ms+ network round-trip time of frontier LLM APIs.

### 5.2 Memory Footprint Under High Concurrency
Go's goroutine runtime allocates a minimal 2KB–4KB initial stack per goroutine, which grows dynamically.
- For **10,000 concurrent active SSE streaming streams**:
  - 10,000 goroutines × 4KB stack ≈ 40 MB.
  - Buffer pools and HTTP connection metadata ≈ 120 MB.
  - Base process runtime & heap ≈ 40 MB.
  - **Total RSS Footprint**: **~200 MB**.
This allows high-density deployment on modest Kubernetes worker nodes (e.g. 1 vCPU / 512MB RAM pods).

### 5.3 Standard Library Networking Maturity
Go's standard library `net/http` is one of the most thoroughly battle-tested networking packages in computer science:
- First-class support for HTTP/1.1 and HTTP/2 transparent multiplexing.
- Native `http.Flusher` interface enabling zero-delay chunk flushing to downstream clients.
- Clean context-driven socket lifecycle management (`req.Context().Done()`), ensuring that dropped client connections immediately terminate upstream provider HTTP sockets, preventing wasted token billing.

---

## 6. Consequences & Operational Mandates

### Positive Consequences
- **Rapid Feature Delivery**: Straightforward Go concurrency patterns allow senior engineers to implement, test, and debug complex routing and policy logic without battling lifetime annotations or async runtime deadlocks.
- **Minimalist Deployment**: Statically linked binary (`CGO_ENABLED=0`) compiled to single executable. Containers can be built `FROM scratch` with no OS packages, resulting in tiny attack surfaces and zero vulnerability patching for base OS packages.
- **Native Ecosystem Synergy**: Direct integration with the official Kubernetes client-go, Prometheus client library, HashiCorp Vault Go SDK, and OpenTelemetry Go SDK.

### Negative Consequences & Operational Mitigations
- **Risk of Excessive Heap Allocations**: Sloppy allocations in JSON deserialization could trigger elevated GC frequency under heavy loads.
  - *Mitigation*: Enforce automated benchmarks (`testing.B`) and allocation profiling (`pprof`) in CI pipelines. Prohibit unpooled byte allocations in the streaming pipeline.
- **CPU Starvation from Heavy Regex DLP**: Heavy regular expressions executed inside goroutines could monopolize OS threads.
  - *Mitigation*: Use standard library `regexp` (which guarantees linear time O(n) via RE2 principles) and cap input payload scanning buffers to 64KB per chunk. Offload complex ML-based classification to dedicated out-of-process sidecars.

---

## 7. Conclusion

Go 1.23+ provides the optimal intersection of performance, operational simplicity, ecosystem maturity, and developer maintainability. It strictly embodies the project philosophy: **boring, reliable, deterministic infrastructure**.
