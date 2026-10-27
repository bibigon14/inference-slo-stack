# vllm-mock

Minimal Go HTTP server that imitates a vLLM 0.6.x inference endpoint.
Emits the same `vllm:*` Prometheus metric families real vLLM does, so
the SLO rules and dashboards in the parent repo validate against
realistic data without needing a GPU.

## Endpoints

- `POST /v1/chat/completions` - streams Server-Sent Events chunks,
  simulated TTFT + per-token TPOT delays drawn from log-normal
  distributions
- `GET /metrics` - Prometheus text format
- `GET /healthz` - 200 OK

## Metrics emitted

| Metric | Type | Labels |
|---|---|---|
| `vllm:time_to_first_token_seconds` | Histogram | model |
| `vllm:time_per_output_token_seconds` | Histogram | model |
| `vllm:request_total` | Counter | model |
| `vllm:request_success_total` | Counter | model, status |
| `vllm:generation_tokens_total` | Counter | model |
| `vllm:num_requests_running` | Gauge | - |
| `vllm:num_requests_waiting` | Gauge | - |
| `vllm:gpu_cache_usage_perc` | Gauge | - |

Bucket boundaries on the histograms match real vLLM 0.6.x defaults.

## Env knobs

| Variable | Default | Effect |
|---|---|---|
| `LISTEN_ADDR` | `:8000` | Listener address |
| `MODEL_NAME` | `mistral-7b-instruct` | Label value on all metrics |
| `TTFT_MEDIAN_MS` | `50` | Log-normal median for time-to-first-token |
| `TTFT_SIGMA` | `1.0` | Log-normal sigma for TTFT |
| `TPOT_MEDIAN_MS` | `20` | Log-normal median for per-output-token latency |
| `TPOT_SIGMA` | `0.7` | Log-normal sigma for TPOT |
| `ERROR_RATE_PCT` | `0.0` | Fraction of requests that return 500 |
| `DEGRADED` | `false` | If `true`, adds +500ms to every TTFT - demonstrates the TTFT burn-rate alert firing |

## Build + run locally

```bash
cd vllm-mock
go mod tidy
go build -o vllm-mock .
./vllm-mock
```

Then in another terminal:
```bash
curl -N -XPOST http://localhost:8000/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"mistral-7b-instruct","messages":[{"role":"user","content":"hi"}],"max_tokens":30}'
curl http://localhost:8000/metrics | grep '^vllm:'
```

## Build + deploy to OCI VM (v0.1 workflow)

Local build on Mac, cross-compile for arm64 (OCI Ampere A1), then
import into k3s containerd on the VM:

```bash
cd vllm-mock
docker buildx build --platform linux/arm64 -t vllm-mock:v0.1 --output type=docker,dest=vllm-mock-v0.1.tar .
scp vllm-mock-v0.1.tar ociv:/tmp/
ssh ociv 'sudo k3s ctr images import /tmp/vllm-mock-v0.1.tar && rm /tmp/vllm-mock-v0.1.tar'
```

v0.2 will push to `ghcr.io/bibigon14/vllm-mock` via GitHub Actions so
the Deployment's `image:` reference just works everywhere.
