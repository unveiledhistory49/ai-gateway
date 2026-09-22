# AI Gateway Operations, Deployment & Runbook Manual

## 1. Overview & Operational Principles

This manual defines deployment architectures, runtime lifecycle management, health checking, and incident response procedures for the AI Gateway (`ai-gateway`). Compliant with `/root/company-project-specs/00-PROJECT-PHILOSOPHY.md`, every operational procedure here is explicit, measurable, and designed for real on-call production environments.

The gateway is stateless at the HTTP routing layer and relies on an external Redis state store for distributed rate limits, while maintaining an embedded in-memory fallback engine for high availability during partitions.

---

## 2. Deployment Topologies

### 2.1. Standalone Linux Binary

For bare-metal and edge virtual machine deployments, the gateway compiles into a single static binary with zero dynamic library dependencies.

#### Kernel Tuning Parameters (`/etc/sysctl.d/99-ai-gateway.conf`)
High-throughput proxying with thousands of concurrent long-lived SSE connections requires kernel socket optimization:

```ini
# Maximum socket listen backlog for high connection bursts
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535

# Ephemeral port range expansion for high outbound provider concurrency
net.ipv4.ip_local_port_range = 1024 65535

# Fast recycling of closed TCP sockets
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 15

# Memory limits for TCP buffers (4KB min, 87KB default, 16MB max)
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216
```

Apply with: `sysctl --system`

---

### 2.2. systemd Service Configuration (`/etc/systemd/system/ai-gateway.service`)

The systemd service unit enforces security sandboxing, high open-file limits, and graceful shutdown timeouts.

```ini
[Unit]
Description=AI Gateway Production Service
Documentation=file:///root/ai-gateway/docs/OPERATIONS.md
After=network-online.target remote-fs.target
Wants=network-online.target

[Service]
Type=simple
User=ai-gateway
Group=ai-gateway
WorkingDirectory=/var/lib/ai-gateway
ExecStart=/usr/local/bin/ai-gateway --config=/etc/ai-gateway/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
KillMode=process
KillSignal=SIGTERM
TimeoutStopSec=90s
Restart=always
RestartSec=5s

# File descriptor limits for concurrent SSE connections
LimitNOFILE=65536
LimitNPROC=32768

# Linux Security Hardening Directives
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
PrivateTmp=yes
PrivateDevices=yes
NoNewPrivileges=yes
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes

# Environment variables
Environment="GATEWAY_ENV=production"
Environment="DRAIN_TIMEOUT_SECONDS=60"
EnvironmentFile=-/etc/ai-gateway/gateway.env

[Install]
WantedBy=multi-user.target
```

---

### 2.3. Kubernetes Pod Architecture

The Kubernetes deployment guarantees zero-downtime rolling updates, zone resilience, and graceful request draining.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ai-gateway
  namespace: ai-gateway-prod
  labels:
    app.kubernetes.io/name: ai-gateway
    app.kubernetes.io/part-of: ai-platform
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 0
  selector:
    matchLabels:
      app.kubernetes.io/name: ai-gateway
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ai-gateway
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: "/metrics"
    spec:
      terminationGracePeriodSeconds: 75
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: topology.kubernetes.io/zone
          whenUnsatisfiable: DoNotSchedule
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: ai-gateway
      containers:
        - name: gateway
          image: ghcr.io/company/ai-gateway:v1.4.2
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/ai-gateway"]
          args: ["--config=/etc/ai-gateway/config.yaml"]
          ports:
            - name: http-ingress
              containerPort: 8080
              protocol: TCP
            - name: metrics
              containerPort: 9090
              protocol: TCP
          env:
            - name: DRAIN_TIMEOUT_SECONDS
              value: "60"
            - name: POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
          lifecycle:
            preStop:
              exec:
                # Sleep 10s before SIGTERM to allow kube-proxy and Ingress to flush endpoints
                command: ["/bin/sleep", "10"]
          resources:
            requests:
              cpu: "2000m"
              memory: "2Gi"
            limits:
              cpu: "4000m"
              memory: "4Gi"
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            runAsUser: 10001
            capabilities:
              drop: ["ALL"]
          startupProbe:
            httpGet:
              path: /healthz/startup
              port: 8080
            initialDelaySeconds: 1
            periodSeconds: 2
            failureThreshold: 15
          livenessProbe:
            httpGet:
              path: /healthz/liveness
              port: 8080
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet:
              path: /healthz/readiness
              port: 8080
            periodSeconds: 2
            timeoutSeconds: 2
            failureThreshold: 2
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: ai-gateway-pdb
  namespace: ai-gateway-prod
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: ai-gateway
```

---

## 3. Zero-Downtime Rolling Deployment & Graceful Shutdown

Because LLM completions frequently stream tokens for 30 to 60 seconds, standard HTTP process termination would sever active user connections, corrupt client state, and waste token investments. The gateway implements an explicit, multi-phase shutdown sequence.

### 3.1. Shutdown Lifecycle Sequence

```
1. Kubernetes initiates rollout or node drain
2. kubelet executes Pod preStop hook:
   - Command: /bin/sleep 10
   - Gateway continues servicing traffic normally.
   - Endpoint Controller removes Pod IP from Endpoints / EndpointSlices.
   - Ingress controller / AWS ALB / Envoy stops routing new connections to this Pod.
3. kubelet sends SIGTERM to Gateway process (PID 1)
4. Gateway enters DRAIN mode:
   - Flips internal atomic state: draining = true.
   - /healthz/readiness probe immediately responds HTTP 503 (Service Draining).
   - Ingress port stops accepting new TCP connections.
5. In-flight Request Draining Phase (Up to DRAIN_TIMEOUT_SECONDS = 60s):
   - Non-streaming requests finish and return responses.
   - Long-lived SSE token streams continue streaming to clients.
   - Internal wait group (sync.WaitGroup) tracks active stream counter.
6. Clean Teardown (< 5s remaining):
   - Flush pending OpenTelemetry spans to collector.
   - Flush metrics buffer.
   - Terminate Redis connection pool.
7. Gateway exits with code 0.
```

---

## 4. Health Check Probes Specification

The gateway provides three distinct endpoints on the HTTP listener to support container orchestrators:

### 4.1. `/healthz/startup`
- **Purpose**: Validates process initialization before handing control to liveness and readiness probes.
- **Checks**:
  - Configuration file parsed and verified.
  - TLS certificates and JWT verification keys loaded into memory.
  - Routing table compiled without cyclic references.
  - Initial Redis ping succeeds or degraded local mode configured.
- **Response**:
  - Success: `HTTP 200 OK`, `{"status":"started","version":"1.4.2"}`
  - Failure: `HTTP 503 Service Unavailable`, `{"status":"initializing","details":"Loading routing tables"}`

### 4.2. `/healthz/liveness`
- **Purpose**: Detects deadlocks, event loop stalls, or internal thread panics.
- **Checks**:
  - Core event loop responsiveness (execution of a non-blocking channel test).
  - Memory heap below hard safety ceiling (e.g. < 90% cgroup limit).
- **Response**:
  - `HTTP 200 OK`, `{"status":"alive"}`
- *Note*: External dependency outages (e.g., upstream provider down or Redis down) **must not** fail liveness. Failing liveness triggers container restarts, which worsens cascade failures.

### 4.3. `/healthz/readiness`
- **Purpose**: Signals whether the instance can currently accept customer traffic.
- **Checks**:
  - `draining == false` (not in shutdown sequence).
  - At least one valid provider path is available in the route table.
  - Policy engine initialized.
  - Redis connection healthy OR local degraded mode fully operational.
- **Response**:
  - Ready: `HTTP 200 OK`, `{"status":"ready","active_streams":42}`
  - Not Ready: `HTTP 503 Service Unavailable`, `{"status":"draining","active_streams":12}`

---

## 5. Operational Runbooks for On-Call Incidents

### Runbook 1: Major Provider Outage (Route Diversion)

#### Severity: CRITICAL
#### Symptoms
- Alert firing: `AIGatewayProviderCircuitBreakerTripped` or `AIGatewayAvailabilityErrorBudgetBurnRate14x`.
- Grafana dashboard shows sharp increase in HTTP 5xx or connection timeouts to `anthropic-claude`.

#### Immediate Actions
1. **Verify Circuit Breaker Status**:
   ```bash
   curl -s http://localhost:9090/admin/v1/breakers | jq .
   ```
   Check if the circuit breaker has transitioned to `OPEN` automatically. If automated failover is executing, monitor fallback capacity on Azure OpenAI or Internal Llama clusters.

2. **Trigger Emergency Manual Route Diversion**:
   If the upstream is returning corrupt payloads or erratic latencies that have not yet tripped the breaker, force an administrative traffic diversion:
   ```bash
   curl -X POST http://localhost:9090/admin/v1/routes/production-chat/override \
     -H "Authorization: Bearer ${GATEWAY_ADMIN_TOKEN}" \
     -H "Content-Type: application/json" \
     -d '{
       "forced_provider": "azure-openai",
       "reason": "Anthropic US-East fiber degradation incident INC-8492",
       "ttl_seconds": 3600
     }'
   ```

3. **Verify Shifted Traffic**:
   Execute PromQL query in Grafana:
   ```promql
   sum(rate(ai_gateway_requests_total{model="production-chat"}[2m])) by (provider, status_code)
   ```
   Confirm that 100% of requests are now routing to `azure-openai` with status `200`.

4. **Rollback Diversion After Upstream Recovery**:
   When provider status is green and test probes succeed:
   ```bash
   curl -X DELETE http://localhost:9090/admin/v1/routes/production-chat/override \
     -H "Authorization: Bearer ${GATEWAY_ADMIN_TOKEN}"
   ```

---

### Runbook 2: Rate Limit Exhaustion / Request Storm

#### Severity: HIGH
#### Symptoms
- Alert firing: `AIGatewayTenantRateLimitStorm`.
- Client applications reporting widespread HTTP 429 errors.

#### Immediate Actions
1. **Identify Top Offending Tenants**:
   Run PromQL query to isolate rogue services or runaway client batch jobs:
   ```promql
   topk(5, sum(rate(ai_gateway_ratelimit_rejections_total[2m])) by (tenant_id, limit_type))
   ```

2. **Inspect Tenant Consumption**:
   Query the gateway tenant inspection API:
   ```bash
   curl -s http://localhost:9090/admin/v1/tenants/analytics-batch-worker/usage | jq .
   ```

3. **Option A: Apply Emergency Throttle to Rogue Tenant**:
   If a rogue background script is impacting shared upstream provider quotas:
   ```bash
   curl -X PATCH http://localhost:9090/admin/v1/tenants/analytics-batch-worker/limits \
     -H "Authorization: Bearer ${GATEWAY_ADMIN_TOKEN}" \
     -H "Content-Type: application/json" \
     -d '{
       "max_concurrency": 5,
       "rpm": 60,
       "tpm": 50000
     }'
   ```

4. **Option B: Grant Temporary Emergency Quota Elevation**:
   If the traffic surge is a legitimate, approved high-priority corporate event:
   ```bash
   curl -X PATCH http://localhost:9090/admin/v1/tenants/checkout-assistant/limits \
     -H "Authorization: Bearer ${GATEWAY_ADMIN_TOKEN}" \
     -H "Content-Type: application/json" \
     -d '{
       "rpm_multiplier": 3.0,
       "duration_minutes": 120
     }'
   ```

---

### Runbook 3: Circuit Breaker Flapping

#### Severity: MEDIUM
#### Symptoms
- Circuit breaker rapidly oscillates between `OPEN`, `HALF-OPEN`, and `CLOSED`.
- Downstream clients experience intermittent 503s alternating with slow 200s.

#### Root Causes
- Upstream error rate hovers directly around the 50% trip threshold.
- The `cooldown_seconds` (30s) is too short for upstream GPU recovery.
- Canary probe traffic is too aggressive, immediately re-tripping the breaker.

#### Immediate Actions
1. **Inspect Transition Rates**:
   ```promql
   rate(ai_gateway_circuit_breaker_transitions_total[5m])
   ```

2. **Adjust Circuit Breaker Damping Parameters**:
   Increase cooldown period and require more consecutive successful probes before closing:
   ```bash
   curl -X PATCH http://localhost:9090/admin/v1/breakers/anthropic-claude \
     -H "Authorization: Bearer ${GATEWAY_ADMIN_TOKEN}" \
     -H "Content-Type: application/json" \
     -d '{
       "cooldown_seconds": 120,
       "half_open_max_probes": 1,
       "required_consecutive_successes": 10,
       "failure_rate_threshold": 0.60
     }'
   ```

3. **Temporary Manual Lock**:
   If flapping causes unacceptable customer instability, lock the breaker in `OPEN` state until provider stabilization is verified:
   ```bash
   curl -X POST http://localhost:9090/admin/v1/breakers/anthropic-claude/lock-open \
     -H "Authorization: Bearer ${GATEWAY_ADMIN_TOKEN}"
   ```

---

### Runbook 4: Redis State Store Partition

#### Severity: HIGH
#### Symptoms
- Alert firing: `AIGatewayStateStorePartition` or `ai_gateway_state_store_errors_total` rising.
- Metric `ai_gateway_state_store_mode` transitions from `0` (DISTRIBUTED) to `1` (DEGRADED_LOCAL).

#### System Behavior
The gateway enters autonomous in-memory token bucket enforcement. Each node enforces `local_limit = global_limit / active_nodes`. Security policies (auth and tenant ACLs) remain strictly enforced.

#### Immediate Actions
1. **Check Redis Connectivity from Gateway Pods**:
   ```bash
   kubectl exec -it deployment/ai-gateway -n ai-gateway-prod -c gateway -- \
     nc -zv redis-cluster.internal 6379
   ```

2. **Inspect Redis Cluster Health**:
   Connect to Redis CLI and check replication status:
   ```bash
   redis-cli -h redis-cluster.internal -p 6379 cluster info
   redis-cli -h redis-cluster.internal -p 6379 ping
   ```

3. **Resolve Partition**:
   - If Redis node failed over, verify DNS/Sentinel records updated.
   - If Redis OOM occurred, scale Redis memory limits or purge expired ephemeral keys:
     ```bash
     redis-cli -h redis-cluster.internal MEMORY USAGE
     ```

4. **Verify Gateway Re-connection**:
   Once Redis connectivity is restored, the gateway background health checker automatically restores distributed coordination within 3 consecutive successful pings. Confirm via metric:
   ```promql
   ai_gateway_state_store_mode == 0
   ```

---

## 6. Disaster Recovery, Chaos Testing & Rollback Procedures

### 6.1. Chaos Engineering Scenarios

To comply with the Quality Standard in `00-PROJECT-PHILOSOPHY.md`, resilience mechanisms must be continuously verified via automated fault injection.

#### Scenario 1: Upstream Latency Injection (Toxiproxy)
- **Injection**: Inject 12,000ms latency on primary provider port.
- **Pass Criteria**:
  1. Primary TTFT timeout trips at exactly 10,000ms.
  2. Gateway cancels primary socket without blocking.
  3. Request seamlessly hedges or fails over to secondary provider.
  4. Client receives valid response within 11,500ms total.

#### Scenario 2: High 503 Provider Surge
- **Injection**: Return HTTP 503 with body `{"error":"overloaded"}` for 100 consecutive requests.
- **Pass Criteria**:
  1. Circuit breaker trips to `OPEN` within 20 requests.
  2. Remaining 80 requests divert to secondary provider with 0ms upstream wait time.
  3. No gateway process crashes or unbounded goroutine growth.

#### Scenario 3: Redis Kill Under Load
- **Injection**: Abruptly kill Redis primary pod (`kubectl delete pod redis-node-0 --force`).
- **Pass Criteria**:
  1. Gateway switches to local memory rate limiting in < 50ms.
  2. No client request receives HTTP 500.
  3. Rate limits remain bounded locally.

---

### 6.2. Automated Rollback Procedures

If a newly deployed gateway version triggers error budget burn:

```bash
# Check current rollout history
kubectl rollout history deployment/ai-gateway -n ai-gateway-prod

# Emergency Rollback to previous known good revision
kubectl rollout undo deployment/ai-gateway -n ai-gateway-prod

# Verify rollback progress
kubectl rollout status deployment/ai-gateway -n ai-gateway-prod
```

#### Automated Rollback Trigger
The CI/CD deployment pipeline executes a canary analysis for 10 minutes following pod rollout. The canary fails and triggers automatic rollback if:
- `histogram_quantile(0.99, sum(rate(ai_gateway_overhead_duration_seconds_bucket[5m])) by (le)) > 0.015`
- OR error rate of canary pods exceeds $0.05\%$ over 5 minutes.
