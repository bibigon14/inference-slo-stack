# Architecture

## Request flow

```
                        OCI Ampere A1 VM (shared cluster)
 +------------------------------------------------------------------------+
 |                           k3s v1.36                                    |
 |                                                                        |
 |  namespace: inference                                                  |
 |  +--------------+       OTLP          +-------------------+            |
 |  | k6 load-gen  |---traces---+        | OTEL Collector    |            |
 |  | (CronJob)    |            |        | (from otel-logs-  |            |
 |  +------+-------+            |        |  traces-stack)    |            |
 |         |                    |        +---------+---------+            |
 |         | HTTP POST /v1/chat |                  |                      |
 |         v                    |                  v                      |
 |  +-----------------+   OTLP  |        +------------------+             |
 |  | vllm-mock (Go)  |---------+        | Tempo            |             |
 |  | Deployment      |         span     | (otel-logs-...)  |             |
 |  | emits:          |---+              +------------------+             |
 |  |  vllm:ttft...   |   | /metrics                                      |
 |  |  vllm:tpot...   |   | scrape                                        |
 |  |  vllm:req_...   |   |                                               |
 |  |  vllm:gpu_...   |   v                                               |
 |  +-----------------+ +---------+     +-------------------+             |
 |                      | VMAgent |---->| VMSingle          |             |
 |                      | (vm ns) |     | (vm-observability)|             |
 |                      +---------+     +---------+---------+             |
 |                                                |                       |
 |                                                v                       |
 |                                    +---------------------+             |
 |                                    | VMAlert evaluates   |             |
 |                                    | VMRule from this    |             |
 |                                    | repo (burn-rate     |             |
 |                                    | alerts, SLO rules)  |             |
 |                                    +---------+-----------+             |
 |                                              | fire                    |
 |                                              v                         |
 |                                    +--------------------+              |
 |                                    | VMAlertmanager     |              |
 |                                    | --webhook-->       |              |
 |                                    +--------------------+              |
 |                                                                        |
 |  namespace: monitoring                                                 |
 |  +------------------+                                                  |
 |  | Grafana          |<--query-- VMSingle + Loki + Tempo                |
 |  | + sidecar reads  |                                                  |
 |  | dashboard CM     |                                                  |
 |  +------------------+                                                  |
 +----------------------------------|-------------------------------------+
                                    | wg0 (WireGuard)
                                    v
                             homelab Pi: alertmanager-telegram-bridge
                                    |
                                    v
                              Telegram bot
```

## Why mock inference instead of real vLLM

Real vLLM needs a CUDA GPU. The cheapest-usable AWS option (g4dn.xlarge,
NVIDIA T4, 16GB) is $0.526/hr - $380/month if left running. The mock
produces the same Prometheus metric families, so every artifact in this
repo (SLO rules, burn-rate alerts, Grafana dashboard, correlation
story) works identically against the real thing. Swapping to real vLLM
in v0.2 is a single Deployment image change.

The mock has three knobs:

- `TTFT_P50_MS`, `TTFT_P99_MS` - log-normal distribution parameters for
  TTFT. Default 50/500ms matches a healthy 7B-13B model on T4.
- `TPOT_P50_MS`, `TPOT_P99_MS` - same for per-output-token latency.
  Default 20/50ms matches ~50 tokens/sec sustained.
- `ERROR_RATE_PCT` - fraction of requests that return 500. Default 0.
  Load generator can override via Deployment env to burn through
  availability budget and demonstrate the burn-rate alert.

## OTEL instrumentation points

Three spans per request:

1. **inference.request** - server root span. Attributes: `model`,
   `prompt_tokens`, `max_tokens`, `temperature`.
2. **inference.prompt_processing** - time from receipt to first token.
   Child of #1. Directly contributes to the TTFT histogram.
3. **inference.generation** - time from first token to completion.
   Child of #1. Span duration / token count feeds TPOT.

Traces land in Tempo via the OTEL Collector from
[otel-logs-traces-stack](https://github.com/bibigon14/otel-logs-traces-stack).
The TraceID is embedded in log lines and metric exemplars so the
Grafana dashboard's trace-to-logs and metric-to-traces links walk the
correlation on click.

## Correlation story on an alert

An alert fires: `InferenceTTFTBurnRateFast{service="llm-chat"}`. The
alert payload carries:

- `summary`: "p99 TTFT burning 14.4x faster than SLO allows"
- `description`: pod name, current p99, SLO target, exemplar trace ID
- `runbook_url`: link to `docs/runbooks/ttft-burn-rate.md` (v0.2)
- `dashboardURL`: link straight to the SLO panel, time-shifted to the
  alert window

Oncall clicks the dashboard link. The Grafana pane shows four panels
reading left-to-right, top-to-bottom:

1. **Burn rate per SLO** (top-left) - which of the three SLOs is on
   fire. Spoiler: TTFT.
2. **p99 latency panels** (top-right) - TTFT + TPOT side by side, 2h
   back. TTFT climbing from 400ms to 800ms.
3. **vLLM resource panels** (bottom-left) - KV cache usage, waiting
   queue depth, running requests. Cache above 90%, queue climbing -
   diagnosis obvious.
4. **Exemplar waterfall** (bottom-right) - trace from Tempo, pinned to
   the exemplar in the TTFT histogram. Shows which prompt took 800ms
   through prompt_processing.

No PromQL at 3am. No open-eight-tabs. The alert carries the context
to its own diagnosis.
