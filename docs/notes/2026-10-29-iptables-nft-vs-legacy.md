# Note: iptables-nft vs iptables-legacy backend mismatch

A short "notes from the field" entry on the specific failure mode that
caused the [2026-10-29 post-reboot CNI cascade](../incidents/2026-10-29-post-reboot-cni-cascade.md).
Not a post-mortem - just the one mechanical thing that chews 3 hours
off your night if you don't already know to look for it.

## The setup

Modern Linux ships two backends for the same `iptables` CLI:

- `iptables-legacy` - writes into the classic `ip_tables` kernel module,
  rules visible via `/proc/net/ip_tables_names`.
- `iptables-nft` - writes into the `nftables` kernel module via an
  `iptables`-compatible translation shim, rules visible via `nft list
  table ip nat`.

`update-alternatives --display iptables` says which one `/usr/sbin/iptables`
resolves to. **The two backends are parallel rule stores. They do not
see each other.**

## How k3s gets broken

k3s embeds kube-proxy; kube-proxy writes its service rules via
`iptables` at startup. If a kernel upgrade (or a routine `apt-get
upgrade`) flips the alternative to `iptables-nft`, but kube-proxy's
compiled-in binding still talks to `iptables-legacy`, kube-proxy
silently writes into one table and every Service lookup reads from
the other. Cilium's init container tries to reach the kube-apiserver
ClusterIP, gets no route, fails, backs off, restarts, same outcome.
From `kubectl` the cluster looks dead: every pod in `ContainerCreating`,
Cilium pods in `Init:Error`, no obvious signal pointing at the
networking plane as the cause.

## The 30-second diagnosis

```bash
sudo iptables -t nat -L KUBE-SERVICES | wc -l
```

Expected output on a working cluster: hundreds of lines (one rule
per Service). Output on a broken cluster: **1** (just the chain
header). If you see 1, the backend is split and kube-proxy's rules
are living in the other store.

Cross-check:

```bash
sudo iptables-legacy -t nat -L KUBE-SERVICES 2>/dev/null | wc -l
sudo iptables-nft    -t nat -L KUBE-SERVICES 2>/dev/null | wc -l
```

One of these will be ~1, the other will be the real rule count.
Whichever has the rules is where kube-proxy actually wrote; the CLI
you've been using is talking to the other.

## The fix

Pin the alternative back to whatever matches your k3s build and
restart the networking layer:

```bash
sudo update-alternatives --set iptables /usr/sbin/iptables-legacy
sudo systemctl restart k3s   # kube-proxy re-writes rules through the
                             # now-correct backend on startup
```

Cilium picks up ~45 seconds later as the apiserver ClusterIP route
starts resolving again.

## Prevention

- Pin the iptables binary before unattended-upgrades touches it:
  `apt-mark hold iptables` on hosts where the backend is known-good.
- Or script a post-boot check that compares
  `iptables -t nat -L KUBE-SERVICES | wc -l` against a threshold and
  pages if the count drops under 10.
- Or drop the ambiguity entirely by switching kube-proxy to IPVS mode
  (`--proxy-mode=ipvs`), which routes through the `ip_vs` kernel
  module and doesn't care about the iptables backend at all.

## Related

- [2026-10-29 post-reboot CNI cascade](../incidents/2026-10-29-post-reboot-cni-cascade.md) - the actual outage this note was extracted from.
- [Debian nft-as-default policy, 2021](https://wiki.debian.org/nftables) - why `iptables-nft` is the modern default.
- [k3s issue tracker: Cilium fails after reboot on kernel 6.x](https://github.com/k3s-io/k3s/issues) - this failure mode has several open reports.
