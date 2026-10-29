# Incident: vmalert CrashLoopBackOff after VMRule apply

**Date:** 2026-10-27 (22:38 PT - detection) → 2026-10-28 (00:15 PT - rollback)
**Duration:** ~1h 37min debug + mitigation
**Severity:** SEV-3 (self-monitoring broken, product metrics unaffected)
**Author:** Dmitry Stepanov

## Summary

Shipping the new `inference.*` VMRule groups for the LLM inference SLO stack
triggered a vmoperator reconcile that rolled vmalert pods. The new pod
could not start because it hit a thundering-herd 429 against vmsingle on
startup. Attempted mitigations introduced a second failure mode (stale CNI
allocation after a force-delete). Final decision was to roll back all
patches rather than fix networking at 23:11 PT the night before a job
interview.

## Impact

- vmalert evaluation stopped for ~90 minutes
- Alert evaluation did not fire during window; recording rules continued to
  evaluate inline in vmagent/vmsingle, so dashboards remained live
- No product-side impact: vllm-mock, k6 generator, and vm-observability
  stack kept scraping and serving
- Only side effect was Telegram self-monitoring noise (`ScrapeTargetMissing`
  for vmalert endpoints)

## Timeline (America/Los_Angeles)

| Time | Event |
|------|-------|
| 2026-10-27 15:52 | `kubectl apply -f manifests/slo-rules/vmrule-inference-slo.yaml` |
| 2026-10-27 15:53 | vmoperator reconciles vmalert Deployment, spawns new ReplicaSet `vmalert-main-68b46f8b4c` |
| 2026-10-27 15:53 | New pod enters startup; MaxSurge=1 keeps old `vmalert-main-5f8b96d4df-nzxxk` running |
| 2026-10-27 15:53+ | vmagent on homelab Pi5 (scraping OCI via WireGuard) sees both vmalert ports down for 10m |
| 2026-10-27 22:28 | `ScrapeTargetMissing` alerts fire to Telegram for `10.42.0.27:8080` and `:8435` |
| 2026-10-27 22:38 | Investigation begins |
| 2026-10-27 22:52 | Root cause confirmed in logs: 429 from vmsingle; `search.maxConcurrentRequests=2` default insufficient for 2 concurrent vmalert pods x 70+ rule groups |
| 2026-10-27 22:55 | Patch vmsingle `extraArgs`: `search.maxConcurrentRequests=16`, `search.maxQueueDuration=60s` |
| 2026-10-27 22:58 | vmsingle Terminating → new pod `vmsingle-main-79c8586f99-t5jb8` starts, stuck 0/1 |
| 2026-10-27 23:04 | Scale vmalert `replicaCount=0` to remove query storm during vmsingle recovery |
| 2026-10-27 23:06 | vmsingle still 0/1 ready after 5m, even with no vmalert load |
| 2026-10-27 23:09 | Memory bump to 2Gi + force-delete stuck pod |
| 2026-10-27 23:11 | New pod `vmsingle-main-6d75f594f4-w9w25` listening on 0.0.0.0:8429 inside container, but `curl` from host and other pods: connection refused |
| 2026-10-27 23:11 | Diagnosis: CNI stale allocation from force-delete left pod network-isolated |
| 2026-10-28 00:14 | Full rollback: `spec.resources=null`, `spec.extraArgs={}`, `vmalert.replicaCount=1`. Decision to defer CNI debug to a clean morning session |

## Root cause

Two independent problems compounded:

1. **Thundering-herd 429**: vmoperator rolling restart creates a window
   where old + new vmalert pods coexist (MaxSurge=1, MaxUnavailable=0).
   Both evaluate the full rule set against the same vmsingle. With
   `-search.maxConcurrentRequests=2` and `-search.maxQueueDuration=10s`
   (vmsingle defaults), the ~70 rule groups starting evaluation at once
   overrun the query queue. The new pod treats 429 responses as fatal and
   enters CrashLoopBackOff before readiness.

2. **CNI stale allocation after force-delete**: `kubectl delete pod
   --grace-period=0 --force` on vmsingle left the previous pod's CNI
   allocation in `/var/lib/cni` with no cleanup. The replacement pod got
   IP 10.42.0.201 allocated but with inconsistent flannel/iptables state.
   Inside the container, the HTTP server bound and accepted on loopback
   (`/proc/net/tcp` showed `00000000:20ED` in state 0A LISTEN).
   From the host (kubelet readiness probe) and from other pods, the pod
   IP timed out at the network layer.

Trigger was benign: a routine `kubectl apply` of a new VMRule. The
system was single-replica tolerant but not rolling-restart tolerant at
the default concurrency.

## Diagnosis path

```
# 1. See the alert symptom
Telegram: ScrapeTargetMissing WARNING - 10.42.0.27:8080 down 10m

# 2. Confirm pod state
$ kubectl -n vm get pods -l app.kubernetes.io/name=vmalert
vmalert-main-5f8b96d4df-nzxxk   2/2   Running             5d11h
vmalert-main-68b46f8b4c-86mmh   0/2   CrashLoopBackOff    6h57m

# 3. The 3 log lines that identified root cause
$ kubectl -n vm logs vmalert-main-68b46f8b4c-86mmh -c vmalert --tail=10
error ... group "inference.alerts" ... rule "InferenceAvailabilityBurnRateFast":
  failed to execute query ... unexpected response code 429 for
  http://vmsingle-main.vm.svc:8429/api/v1/query?query=...
  Response body couldn't start executing the request in 10.000 seconds,
  since -search.maxConcurrentRequests=2 concurrent requests are executed.
  Possible solutions: ... to increase -search.maxConcurrentRequests

# 4. Confirm vmsingle network isolation after memory bump + force restart
$ POD=$(kubectl -n vm get pod -l app.kubernetes.io/name=vmsingle -o jsonpath='{.items[0].metadata.name}')
$ kubectl -n vm exec $POD -- grep ':20ED ' /proc/net/tcp
0: 00000000:20ED 00000000:0000 0A ...   # listening

$ kubectl -n vm exec $POD -- wget -qO- -T 3 http://127.0.0.1:8429/health
OK

$ curl -sm 3 http://10.42.0.201:8429/health   # from host
# (empty, timeout)
```

## Mitigation attempted

| Step | Action | Result |
|------|--------|--------|
| 1 | Delete old vmalert pod to unstick rollout | vmoperator recreated same-version pod, both still crashing |
| 2 | Patch VMSingle `extraArgs` with `maxConcurrentRequests=16` | Patch accepted, vmsingle recreated with new ReplicaSet but readiness probe stuck |
| 3 | Scale `vmalert.replicaCount=0` to remove load during vmsingle recovery | Vmalert pods terminated, but vmsingle still 0/1 |
| 4 | Memory bump `limits.memory=2Gi` + force-delete stuck vmsingle | New pod started with 2Gi, server listening on `:8429` inside container but unreachable from outside |
| 5 | Diagnose CNI stale allocation after force-delete | Pod isolated at network layer |
| 6 | Full rollback | `spec.resources=null`, `spec.extraArgs={}`, `vmalert.replicaCount=1` restored to pre-incident state |

## Resolution

Decision at 23:11 PT: **roll back, don't fix forward**. CNI debug at 23:11
on the night before a hiring manager interview was a bad-decisions trap.
Rollback restored the pre-incident configuration; the system healed
overnight as vmoperator reconciled remaining state against stock defaults.

Confirmed healthy next morning. No data loss. Alert evaluation gap: ~90
minutes. No product impact.

## Lessons

1. **vmsingle default `-search.maxConcurrentRequests=2` is too tight for a
   stack with 2 vmalert replicas evaluating 70+ rule groups at scheduler
   boundaries.** Even outside restart windows, concurrent group evaluation
   can race the queue. Production baseline should be `8-16` with
   `-search.maxQueueDuration=30s-60s`.

2. **Force-delete is a networking hazard on CNI backends that lack
   aggressive cleanup.** For flannel on k3s specifically, `kubectl delete
   --grace-period=0 --force` leaves `/var/lib/cni` state that the next
   pod inherits incorrectly. Prefer normal delete + wait, or
   `kubelet` eviction, over `--force`.

3. **Rolling restart + default concurrency = thundering herd.** Any
   stateful backend with a request queue has this failure mode during
   deploys. The fix is either raise concurrency to cover 2x the normal
   steady-state load, or stagger evaluation (`eval_offset` per rule
   group), or set `vmalert.replicaCount=1` and accept the HA tradeoff.

4. **"Debugging infrastructure at 11pm before an interview = bad
   decisions"** is a real heuristic, not a joke. Rolling back lost
   nothing; fixing forward at that hour would have risked compounding the
   failure mode on an already-shaky CNI state.

5. **Observability holds the system together during its own repairs.**
   Dashboards stayed live (recording rules evaluate in vmagent/vmsingle,
   not vmalert), so I could watch the burn-rate panels confirm no product
   impact while triaging the alert path itself. The alerts failed but the
   data pipeline that feeds them did not — a worthwhile separation of
   concerns.

## Follow-up actions

- [ ] Raise `search.maxConcurrentRequests` to `8` in VMSingle CRD as a
      permanent production baseline (not just this incident)
- [ ] Add `eval_offset` to the heaviest rule groups to stagger evaluation
      and reduce queue bursts
- [ ] Document the force-delete CNI hazard in runbook
- [ ] Consider `vmalert.replicaCount=1` for this homelab-adjacent
      deployment — 2 replicas is unnecessary on a single-node OCI VM
      and doubles the vmsingle query load for no durability gain
- [ ] Add a self-monitoring recording rule that fires when `vmsingle`
      queue depth is >50% of `maxConcurrentRequests` for 5m, so the
      next thundering-herd shows up as a leading indicator rather than
      a scrape-target-missing lagging signal

## References

- [VictoriaMetrics docs: Resource usage limits](https://docs.victoriametrics.com/#resource-usage-limits)
- [Google SRE Workbook ch.5: Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/)
- `manifests/slo-rules/vmrule-inference-slo.yaml` - the VMRule that
  triggered the reconcile
