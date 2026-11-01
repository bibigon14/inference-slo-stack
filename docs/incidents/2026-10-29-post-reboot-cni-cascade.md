# Incident: post-reboot CNI cascade - iptables backend mismatch

**Date:** 2026-10-29 21:30 PT (detection) → ongoing partial resolution
**Severity:** SEV-2 (cluster-wide CNI failure, all workloads stuck)
**Status:** Root cause identified and primary fix applied. Three
VictoriaMetrics operator-managed workloads (vmoperator, vmagent,
vmalertmanager) remain in CrashLoopBackOff pending chain-recovery.
**Author:** Dmitry Stepanov
**Follow-up to:** [2026-10-27 vmalert CrashLoopBackOff](2026-10-27-vmalert-crashloop.md)

## Summary

A node reboot taken to clear leftover state from the 2026-10-27
incident silently landed on a newer kernel that OCI had staged via
unattended-upgrades. The upgraded kernel's nftables subsystem was
incompatible with the installed iptables userspace binary, which
caused k3s to fail every attempt to populate iptables rules. With no
rules in place, the Kubernetes service IP (10.43.0.1:443) stopped
routing, Cilium could not reach the API server to initialise, and
every pod across the cluster got stuck in `ContainerCreating` with
`FailedCreatePodSandBox` for 10+ minutes.

## Impact

- **Cluster-wide CNI failure for ~1 hour** (22:00-23:00 PT 10-29)
- All `ContainerCreating` pods stuck - no new workloads could start
- vmsingle + vmalert eventually recovered after root-cause fix
- vmoperator / vmagent / vmalertmanager still in CrashLoopBackOff
  48h later (see Open Items below) - these pods were created during
  the broken-CNI window and inherited stale network namespace state
  that fresh recreation has not yet cleared
- Product-side (vllm-mock, k6 load generator) recovered fully

## Timeline (America/Los_Angeles)

| Time | Event |
|------|-------|
| 2026-10-29 21:36 | Noticed vmsingle `0/1 Ready` 46h, vmalert in `CrashLoopBackOff` 1667 restarts - lingering state from 2026-10-27 incident never fully recovered |
| 2026-10-29 21:42 | `kubectl delete pod vmsingle` + `k3s restart` - normal pod replace did not clear issue |
| 2026-10-29 21:47 | Decided on node reboot (`sudo reboot`) to clear all stale state |
| 2026-10-29 21:49 | Node back online, k3s API responsive within 60s |
| 2026-10-29 21:50 | **All pods stuck in `Unknown`/`Completed` status with 7-11 day AGE** - kubelet not recreating replacements |
| 2026-10-29 21:52 | Force-deleted stale pods to trigger ReplicaSet recreation |
| 2026-10-29 21:55 | New pods stuck in `ContainerCreating` 4+ min with `FailedCreatePodSandBox: plugin type="cilium-cni" failed - unable to connect to Cilium agent` |
| 2026-10-29 21:56 | Cilium agent itself in `Init:Error`, operator in CrashLoopBackOff - CNI dead cluster-wide |
| 2026-10-29 22:14 | Dug into Cilium config init logs: `"Unable to contact k8s api-server: Get https://10.43.0.1:443/api/v1/namespaces/kube-system: dial tcp 10.43.0.1:443: i/o timeout"` - chicken-and-egg between CNI and API routing |
| 2026-10-29 22:15 | Checked iptables: `iptables -t nat -L KUBE-SERVICES \| wc -l` returned **1** (header only). k3s had not created ANY iptables rules |
| 2026-10-29 22:30 | Checked journal: `iptables v1.8.10 (nf_tables): Parsing nftables rule failed. Perhaps iptables or your kernel needs to be upgraded.` repeating every 30s |
| 2026-10-29 22:33 | Applied fix: `update-alternatives --set iptables /usr/sbin/iptables-legacy` + `systemctl restart k3s` |
| 2026-10-29 22:34 | iptables rules populated (14 lines in KUBE-SERVICES), Cilium reached 1/1 Ready, vmsingle + vmalert back to Running |
| 2026-10-29 22:40 | Three vmoperator-managed pods (vmagent, vmalertmanager, vmoperator) still CrashLoop - their first containers started during the broken window and had cached the dead service IP in their config loaders |

## Root cause

Three independent factors compounded into a cluster-wide stall:

1. **Kernel upgrade via `unattended-upgrades`**: between the 2026-10-27
   incident and the 2026-10-29 reboot, OCI's security updates had
   staged a kernel bump. The reboot activated it. The new kernel's
   `nf_tables` subsystem had a slightly different rule format from
   what `iptables v1.8.10` (userspace) was built against.

2. **iptables userspace / kernel version drift**: `iptables-nft`
   (the default on Ubuntu 24.04) uses `nf_tables` as backend but
   expects a specific rule format. The parse failures meant **every
   rule k3s tried to install was rejected** with `Parsing nftables
   rule failed`. The error was logged, not raised - k3s kept running
   with no network rules in place.

3. **k3s's embedded kube-proxy**: because the embedded proxy was the
   only thing creating iptables rules for Kubernetes services, its
   silent failure meant `10.43.0.1:443` had no NAT rule. This made
   the Kubernetes API service IP unreachable **from inside any pod**,
   even though the API server itself was healthy on `10.0.1.79:6443`.

4. **CNI chicken-and-egg**: Cilium's config init container needs to
   reach the API server (via `10.43.0.1:443` by default) to pull
   configuration before the main agent can start. With that path
   broken, Cilium never started. Without Cilium, no pod could get a
   network namespace. The cluster deadlocked.

The post-reboot `Unknown`/`Completed` pod states that appeared
first were a separate kubelet reconciliation symptom of the same
underlying CNI failure.

## Diagnosis path

The breakthrough came from one command:

```bash
sudo iptables -t nat -L KUBE-SERVICES | wc -l
# returned: 1
```

For a healthy k3s cluster this should be in the double digits (my
cluster showed 14 after fix). A value of 1 means **only the chain
header exists, no rules**. That immediately pointed away from CNI
(which was the obvious suspect given the error messages) and toward
whatever creates iptables rules - which on k3s is embedded
kube-proxy.

Following that:

```bash
sudo journalctl -u k3s --since "10 minutes ago" | grep -iE "iptables|parsing"
```

Surfaced the actual error:

```
iptables v1.8.10 (nf_tables): Parsing nftables rule failed
Perhaps iptables or your kernel needs to be upgraded.
```

With that in hand the fix was a known one for Ubuntu + k3s + nftables
backend mismatch.

## Fix

```bash
sudo update-alternatives --set iptables /usr/sbin/iptables-legacy
sudo update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy
sudo systemctl restart k3s
```

Within 60 seconds:
- `KUBE-SERVICES` chain populated with 14 rules
- `10.43.0.1:443` routing worked
- Cilium config init completed, agent reached 1/1 Ready
- vmsingle and vmalert pods (which had been retrying throughout)
  succeeded on next restart and reached Ready

## Open items (as of 2026-10-31)

Three VictoriaMetrics operator-managed pods are still in
CrashLoopBackOff 514 restarts later:
- `vmoperator-victoria-metrics-operator`
- `vmagent-main`
- `vmalertmanager-main-0`

They first started during the broken-CNI window and have cached
stale network state. A naive force-delete has not cleared it. Full
recovery requires an ordered chain-restart of the three with the
operator going first so the two managed objects can rebuild their
config secrets against a working control plane. Deferred to a clean
morning session; vmsingle and vmalert covering the critical SLO
path keep the stack functional in the interim.

## Lessons

1. **"CNI failure" with `connection refused` from inside pods is
   not always a CNI problem.** When the API server is unreachable
   from pods *and* from the host via its service IP, trace back
   through iptables before deep-diving CNI. The one-liner
   `iptables -t nat -L KUBE-SERVICES | wc -l` would have shaved 45
   minutes off this triage.

2. **Unattended-upgrades on infrastructure nodes is a loaded
   footgun.** The kernel bump that triggered this was transparent
   until a reboot. For single-node clusters at least, `apt-mark hold
   linux-image-*` and manual upgrade windows prevent exactly this
   class of surprise. Multi-node clusters get the same protection
   from rolling restarts hitting one node at a time.

3. **iptables-nft vs iptables-legacy is a live compatibility matrix,
   not a solved problem.** k3s's own docs call this out but the
   failure mode (silent rule-rejection, service IPs quietly
   un-routable) is far from the loud, obvious error you'd expect.

4. **Chicken-and-egg between CNI and service IPs is a real failure
   mode.** Cilium specifically supports `k8s-service-host` /
   `k8s-service-port` as a config override exactly for this
   bootstrap case - pointing to the real API server IP instead of
   the service IP. Worth setting as a defensive default even on
   single-node clusters where the service IP "always works" today.

5. **Multi-stage incidents are the hardest.** This was traceable
   back through the 10-27 vmalert incident -> force-delete of
   vmsingle leaving stale CNI allocation -> reboot to clear it ->
   kernel upgrade activating silently -> iptables parse failures.
   Each step was defensible on its own; the compound was not.

## Follow-up actions

- [ ] Resolve the three CrashLooping vmoperator pods (ordered
      chain-restart with operator first)
- [ ] `apt-mark hold linux-image-*` on this VM to prevent silent
      kernel bumps
- [ ] Add `k8s-service-host` / `k8s-service-port` to the Cilium
      ConfigMap as a bootstrap defence
- [ ] Add a monitoring rule that alerts when `iptables -t nat -L
      KUBE-SERVICES` has fewer than 5 rules - leading indicator for
      exactly this failure mode
- [ ] Consider pinning iptables to legacy mode on all homelab-like
      single-node k3s installs

## References

- [k3s GitHub issue - iptables nft/legacy compatibility](https://github.com/k3s-io/k3s/issues/8912)
- [Cilium docs - Kube-proxy replacement bootstrap](https://docs.cilium.io/en/stable/network/kubernetes/kubeproxy-free/#kubernetes-api-server-connectivity)
- Previous incident: [2026-10-27 vmalert CrashLoopBackOff](2026-10-27-vmalert-crashloop.md)
