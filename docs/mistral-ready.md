# Reading guide (interview-ready)

A reader with 60 seconds should walk away understanding three things:

**1. The SLI set I picked for LLM inference and why there are three
of them, not one.**

Latency on a chat completion is the sum of two orthogonal things:
*time to first token* (TTFT, dominated by prompt-processing - prompt
tokenization, KV cache prefill, scheduler contention) and *time per
output token* (TPOT, dominated by per-step decode cost). A single
end-to-end p99 latency SLI lumps them together and hides which half
of the pipeline is in trouble. Add availability (fraction of
non-5xx) as the third SLI because TTFT and TPOT are undefined on
requests that never completed. That gives three SLIs, each with
their own SLO, each with their own burn-rate alert chain. The full
PromQL and reasoning is in [slo-vocabulary.md](slo-vocabulary.md).

**2. The alerting pattern comes straight from the Google SRE
Workbook, not from me.**

Each SLO gets *two* burn-rate alerts, not one. A fast burn
(1h × 14.4 threshold) pages for an outage big enough to consume a
week's budget in under 5 hours. A slow burn (6h × 6 threshold)
tickets for a creeping degradation that would consume the budget in
~16h. The multi-window double-check (short window confirms the long
window) cuts noise so a single bad minute doesn't page. The
full rule set is in [`manifests/slo-rules/vmrule-inference-slo.yaml`](../manifests/slo-rules/vmrule-inference-slo.yaml) -
all six alerts (3 SLOs × 2 severities) read like one copy of the same
pattern parameterized by SLI name and threshold, which is how a real
SLO rule library should look.

**3. The correlation story on an alert is the actual deliverable.**

An alert that only says "p99 TTFT is high" is a notification, not a
tool. The alert in this repo lands with the trace exemplar of a slow
request, the pod name, and a direct Grafana link that opens the SLO
dashboard pre-filtered to the right time window. From there, two
clicks: see the burn-rate panel confirm which SLO is actually
burning (not all three - TTFT alone), see the vLLM internals panel
(KV cache, queue depth, running requests) which usually gives the
diagnosis in one glance. The architecture of that walk is in
[architecture.md](architecture.md).

## What I did NOT optimize for (deliberately)

- **Running real vLLM on a real GPU** - a g4dn.xlarge at $0.526/hr
  is $380/month to keep live, and nothing in this repo's SLO or
  dashboard logic cares whether the metrics come from the real
  runtime or from a mock that emits the same metric families.
  Swapping to real vLLM is a Deployment image change. See
  [gpu-observability-eks](https://github.com/bibigon14/gpu-observability-eks)
  for the GPU-side companion (DCGM, nvidia-device-plugin, real vLLM
  on T4 - stood up, screenshotted, torn down).
- **A full LLM gateway with per-model routing and cost SLIs** - that
  is a different project (v0.3 of this repo, probably). The scope
  here is the single-endpoint SLO story. Getting that one right is
  the prerequisite.
- **Writing recording rules by hand for every window** - a mature
  library would be generated from a `pyrra` or `slo-generator` spec.
  Hand-written here on purpose so the rules are reviewable in one
  file, not generated from a schema the reader has to go learn.
