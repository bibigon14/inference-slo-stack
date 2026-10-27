# SLO vocabulary for LLM inference

Operators of LLM inference endpoints get asked three questions in every
oncall discussion:

1. *Are we slow?*
2. *How much error budget did that incident eat?*
3. *When do we page?*

Answering those with "p99 latency went up" is wrong - a single
p99-latency number for a chat completion means nothing, because that
latency is the sum of two orthogonal things with different causes and
different remediations. This document establishes the vocabulary this
repo uses, in a form that transfers straight to a production LLM
platform.

## The three SLIs

### 1. Availability (request-success rate)

**Definition**: fraction of inference requests that return a non-5xx
response within a hard timeout (default 30s).

**Why it is first**: a 500 or a 30s timeout is a user-visible outage.
TTFT and TPOT only matter on requests that complete at all.

**Prometheus expression**:
```
sum(rate(vllm:request_success_total[5m]))
/
sum(rate(vllm:request_total[5m]))
```

**SLO**: **99.9% over 7 days** rolling. 7d is right for LLM inference
because deploys are frequent, a 30d window hides week-to-week regression,
and a 1h window pages too often on single-request outliers.

### 2. Time to first token (TTFT)

**Definition**: time from receiving a request to emitting the first
output token. Dominated by *prompt-processing* work: tokenization, KV
cache prefill for the prompt, batching latency against other
in-flight requests.

**Why it is separate from TPOT**: TTFT scales with *prompt* length;
TPOT scales with *response* length. Lumping them hides which half of
the pipeline is in trouble. A TTFT regression says "prompt processing
is slow" - think bigger prompts, cold KV cache, scheduler contention. A
TPOT regression says "generation is slow" - think GPU memory pressure,
model offloading, inefficient batching of streaming responses.

**Prometheus expression** (p99):
```
histogram_quantile(0.99,
  sum by (le) (rate(vllm:time_to_first_token_seconds_bucket[5m]))
)
```

**SLO**: **p99 TTFT < 500ms over 7 days**. The 500ms number is the
upper bound of "feels instant" in human-computer-interaction research
and the number vLLM's own benchmarks target for 7B-13B models on
single-GPU inference.

### 3. Time per output token (TPOT)

**Definition**: for a request that produced N output tokens in T
seconds, TPOT = T / N. Dominated by *per-step* decode cost: a single
transformer forward pass + sampling + token streaming overhead.

**Prometheus expression** (p99):
```
histogram_quantile(0.99,
  sum by (le) (rate(vllm:time_per_output_token_seconds_bucket[5m]))
)
```

**SLO**: **p99 TPOT < 50ms over 7 days**. 50ms per token corresponds
to roughly 20 tokens/sec, which is just above typical human reading
speed (~15 tokens/sec for English prose). Users notice the moment it
drops below ~15 tokens/sec.

## Error-budget math

Each SLO defines an error budget per rolling window.

- Availability SLO 99.9% over 7d → error budget = 0.1% × 7d = **10
  minutes 4.8 seconds of fully-failed requests per week**.
- p99 TTFT SLO over 7d → budget = 0.1% × 7d = **604.8 seconds of time
  spent above the 500ms threshold per week**.
- p99 TPOT SLO over 7d → same math, 604.8 seconds above threshold.

The *recording rule* that computes "fraction of error budget burned so
far this week" lives in `manifests/slo-rules/vmrule-inference-slo.yaml`
and is what the dashboard "Error Budget Remaining" panel displays.

## Burn-rate alerts (multi-window multi-burn-rate)

The multi-window multi-burn-rate pattern comes from Chapter 5 of the
Google SRE Workbook ("Alerting on SLOs"). The problem it solves: a
single-threshold alert either pages on single-request noise (if the
threshold is low) or lets a slow-burning outage eat half the budget
before anyone notices (if the threshold is high).

The fix is to fire only when **two** windows agree the burn rate is
dangerous.

| Severity | Short window | Long window | Burn rate | Time to exhaust budget |
|---|---:|---:|---:|---:|
| **page** (fast burn) | 5m | 1h | 14.4 | ~5 hours |
| **ticket** (slow burn) | 30m | 6h | 6 | ~16 hours |

Thresholds are standard SRE Workbook values for a 7d SLO. The math:
burn rate 14.4 means you are burning the week's budget at 14.4x the
sustainable pace, so a full week's budget would be gone in 7d / 14.4 ≈
11.7 hours; the alert fires after 1h of sustained burn to give a 5h
window to respond.

## What this looks like in practice

An alert `InferenceTTFTBurnRateFast` fires. It carries the burning SLO
name (TTFT, availability, TPOT), the pod label of the inference
server, and the trace exemplar of a request that was slow. Oncall
clicks the Grafana link in the alert:

1. Lands on the "LLM Inference SLO" dashboard, time-shifted to -2h to
   -now so the burn curve is in frame
2. Burn-rate panel shows which SLO is actually burning (TTFT in this
   case) - the other two can be fine
3. Correlation panel (right-hand side) shows `vllm:gpu_cache_usage_perc`
   above 0.9 and `vllm:num_requests_waiting` climbing - classic KV
   cache pressure
4. Click an exemplar on the TTFT histogram, follow trace to Tempo,
   see the span waterfall for one slow request

No tab-hopping, no PromQL at 3am, no "what does the SLO even say?".
That is the point of writing it down.
