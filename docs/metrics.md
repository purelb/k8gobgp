# k8gobgp metrics reference

> **Status: current as of gobgp-netlink v1.3.5.** The metrics below are what the two endpoints
> emit today. Anything still marked **(new)** shipped with v1.3.1 and exists now; anything under
> [Removed metrics](#removed-metrics) is gone.
>
> This document described a *target state* for a release that has since shipped, and the
> "(new)"/"removed" markers were read the wrong way round for three releases. If you are updating
> it, re-measure against a running daemon rather than editing the prose - see the family counts
> below for how.

A k8gobgp pod runs two processes, and each exposes its own metrics on its own port. They are not
merged, proxied or consolidated — they are two scrape targets with two distinct metric
namespaces.

| Endpoint | Port | Path | Namespace | Emitted by |
|---|---|---|---|---|
| Controller | `7473` | `/metrics` | `k8gobgp_*` | the k8gobgp manager |
| BGP daemon | `7475` | `/metrics` | `bgp_*`, `fsm_loop_*` | gobgp-netlink (`gobgpd`) |

Port `7474` serves health probes (`/healthz`, `/readyz`), not metrics.

Both endpoints are plain HTTP with no authentication, and the pod runs with `hostNetwork: true`,
so both are reachable at `<nodeIP>:<port>` from anything that can route to the node — including
the BGP fabric. Kubernetes NetworkPolicy does not apply to host-namespace ports. Treat the
contents as public within your node network: peer addresses, ASNs, router IDs, route counts,
build identity, and which sessions have TCP-MD5 configured.

**Rule of thumb for which endpoint to look at:** `k8gobgp_*` answers *"is the controller doing its
job?"* — did reconciliation succeed, is the CR valid, can it reach the daemon. `bgp_*` answers
*"what is BGP actually doing?"* — sessions, routes, BFD, kernel FIB programming.

---

## Controller metrics (port 7473)

### Reconciliation

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `k8gobgp_reconcile_total` | Counter | `name`, `namespace`, `result` | `result` is one of `success`, `failed`, `validation_failed`, `connection_failed`, `duplicate_ignored`. **`duplicate_ignored` is not a failure** — a second BGPConfiguration in the namespace is ignored by design, so `result!="success"` over-counts |
| `k8gobgp_reconcile_duration_seconds` | Histogram | `name`, `namespace` | `ExponentialBuckets(0.001, 2, 15)` → 1ms to ~16s |
| `k8gobgp_configuration_ready` | Gauge | `name`, `namespace` | 1 when the CR reconciled cleanly |
| `k8gobgp_configured_objects` **(new)** | Gauge | `kind`, `name`, `namespace` | What the CR asks for **on this node**, after `nodeSelector` filtering. `kind` is one of `neighbor`, `peer_group`, `dynamic_neighbor`, `vrf`, `policy`, `defined_set`. Replaces six separate `*_configured` gauges |
| `k8gobgp_peer_apply_errors_total` **(new)** | Counter | `key`, `op` | `AddPeer`/`UpdatePeer` failures. **This is the only signal for a peer that gobgpd refused** — a rejected neighbour never appears in `ListPeer`, so no `bgp_*` metric describes it |
| `k8gobgp_cleanup_retries_total` | Counter | `name`, `namespace` | Retries during finalizer cleanup |
| `k8gobgp_cleanup_duration_seconds` | Histogram | `name`, `namespace` | `ExponentialBuckets(0.01, 2, 10)` |

The pairing of `k8gobgp_configured_objects{kind="neighbor"}` against `count(bgp_peer_state)` is
the check for *"the CR asked for N neighbours and gobgpd has fewer"*, which is how a refused peer
becomes visible.

### Daemon connectivity

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `k8gobgp_gobgpd_connection_status` | Gauge | `endpoint` | 1 when a `GetBgp` RPC succeeded. **Driven by the `gobgpd` readiness check**, so it is as fresh as the probe cadence (10s) and reflects a real RPC, not a client handle |
| `k8gobgp_gobgpd_connection_errors_total` | Counter | `endpoint` | Incremented by the reconciler when it cannot reach the daemon |

### BGP RIB

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `k8gobgp_rib_routes` | Gauge | `family` | Route prefixes in the global RIB. **No gobgp-netlink equivalent** — every fork collector is per-peer and none calls `GetTable`. Label values use underscores (`ipv4_unicast`); the fork uses hyphens (`ipv4-unicast`) — see [Label traps](#label-traps) |

### Collection health

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `k8gobgp_metrics_collection_duration_seconds` | Histogram | — | Time to collect RIB stats from gobgpd |
| `k8gobgp_metrics_collection_errors_total` | Counter | — | Collection failures |

### Router ID resolution

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `k8gobgp_router_id_resolution_total` | Counter | `result` | `success` or `failure` |
| `k8gobgp_router_id_resolution_duration_seconds` | Histogram | — | `ExponentialBuckets(0.001, 2, 12)` → **highest finite bound 2.048s**. A `histogram_quantile` alert threshold above 2.048 can never fire |
| `k8gobgp_router_id_info` | Gauge | `router_id`, `source`, `node`, `asn`, `name`, `namespace` | Always 1; read the labels. `source` is `explicit`, `template`, `node-ipv4` or `hash-from-node-name` |
| `k8gobgp_global_restart_required` | Gauge | `field`, `name`, `namespace` | 1 when a `global.*` setting in the CR differs from the running gobgpd. Every compared field gets a series, set to 0 rather than deleted when it stops drifting, so an alert can use `> 0` without worrying about absent series. `field` is bounded to the known `Global` field names. Cleared on CR deletion |

### BGPNodeStatus reporter

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `k8gobgp_nodestatus_write_total` | Counter | `result` | `success`, `error` or `skipped` |
| `k8gobgp_nodestatus_collection_duration_seconds` | Histogram | — | |
| `k8gobgp_nodestatus_last_successful_write_timestamp_seconds` | Gauge | — | Alert on staleness: `time() - <this> > 300`. **Not** a readiness condition — a slow apiserver should not de-schedule a pod whose BGP is healthy |
| `k8gobgp_nodestatus_object_size_bytes` | Gauge | — | Approximate serialized size. Use this to choose truncation caps rather than guessing |

Also present on 7473: controller-runtime's `controller_runtime_*`, `workqueue_*`,
`rest_client_*`, plus Go runtime and process metrics. `workqueue_depth` and `workqueue_adds_total`
are what distinguish "the reconciler is busy because events are arriving" from "the reconciler is
busy because it is churning".

---

## BGP daemon metrics (port 7475)

**36 metric families** are emitted by gobgp-netlink itself: 27 `bgp_*` and 9 `fsm_*`. Measured
against v1.3.5 by scraping the endpoint directly:

```
curl -s http://127.0.0.1:7475/metrics | grep '^# HELP' | awk '{print $3}' | sed 's/_.*//' \
  | sort | uniq -c | sort -rn
```

The endpoint serves 75 families in total; the other 39 are the standard Go runtime (29),
`process_*` (9) and `promhttp_*` (1) collectors that come with any Go Prometheus client.

The total is **configuration-dependent** - per-peer families only appear once peers exist, so the
same daemon served 75 with no peers and 79 with one peer and one peer group. Quote the 36 if you
need a fixed number; do not assert a total.

### Session state, per peer

| Metric | Type | Labels |
|---|---|---|
| `bgp_peer_state` | Gauge | `peer`, `session_state`, `admin_state` |
| `bgp_peer_asn` | Gauge | `peer`, `router_id` |
| `bgp_peer_local_asn` | Gauge | `peer`, `router_id` |
| `bgp_peer_type` | Gauge | `peer` — 0 internal, 1 external |
| `bgp_peer_established_timestamp_seconds` | Gauge | `peer` |
| `bgp_peer_flop_count` | Gauge | `peer` |
| `bgp_peer_out_queue_count` | Gauge | `peer` |
| `bgp_peer_password_set` | Gauge | `peer` — 1 when TCP-MD5 is configured |
| `bgp_peer_send_community` | Gauge | `peer` — **absent when unconfigured**, see below |
| `bgp_peer_remove_private_as` | Gauge | `peer` |

**`bgp_peer_send_community` is absent for peers that do not set it**, as of gobgp-netlink
v1.3.1. Before that it read a constant `0` for every peer in every deployment, because nothing
ever wrote the underlying state — and `0` means *standard*, so a dashboard built on the old
value was reporting the entire fleet as filtering down to standard communities. Those panels go
to no-data rather than silently changing meaning, which is the intended outcome.

Treat it as reporting **configuration, not effect**. The setting is inert for a peer whose
families are all VPN/EVPN/FlowSpec/MUP/VPLS, and for route-server clients; the metric still
reports what was configured. See [`sendCommunity`](../README.md#send-community) for what
survives each setting.

`bgp_peer_state` is an info-style metric: one series per peer, value always 1, with the current
state carried in the `session_state` label. **It does not emit a zero series for states the peer
is not in**, so `count(bgp_peer_state{session_state="..."}) == 0` matches an empty vector and can
never fire. Use `unless` — see [Common queries](#common-queries).

### Routes, per peer per family

| Metric | Type | Labels |
|---|---|---|
| `bgp_routes_received` | Gauge | `peer`, `route_family` |
| `bgp_routes_accepted` | Gauge | `peer`, `route_family` |
| `bgp_routes_advertised` | Gauge | `peer`, `route_family` |

`bgp_routes_advertised` is the expensive one — computing it walks the RIB per peer per family
with export policy applied. It can be turned off with `--metrics-advertised-routes-disable`.

### Message counters, per peer

18 counters: `bgp_received_*` and `bgp_sent_*` for `update`, `notification`, `open`, `refresh`,
`keepalive`, `withdraw_update`, `withdraw_prefix`, `discarded` and `message` (the total). All
labelled `peer`.

`bgp_{received,sent}_notification_total` are the two worth keeping if you filter the rest — a
NOTIFICATION is how a session tells you why it dropped.

### BFD, per peer

| Metric | Type | Labels |
|---|---|---|
| `bgp_peer_bfd_enabled` | Gauge | `peer` — emitted for every peer, so "configured but not up" is a comparison, not a missing series |
| `bgp_peer_bfd_state` | Gauge | `peer`, `session_state`, `remote_session_state` — always 1 |
| `bgp_peer_bfd_transmitted_packets_total` | Counter | `peer` |
| `bgp_peer_bfd_received_packets_total` | Counter | `peer` |
| `bgp_peer_bfd_failure_transitions_total` | Counter | `peer` |

### BFD server

| Metric | Type | Notes |
|---|---|---|
| `bgp_bfd_server_up` | Gauge | Socket bound (1) or not (0). Binds lazily on the first BFD peer, so 0 is normal when nobody uses BFD |
| `bgp_bfd_received_packet_total` | Counter | Matched to a peer |
| `bgp_bfd_received_drop_total` | Counter | Peer's receive queue was full — the queue is one packet deep, so any advance means an event loop stalled longer than a transmit interval |
| `bgp_bfd_received_error_total` | Counter | Socket read errors |
| `bgp_bfd_invalid_packet_total` | Counter | Undecodable datagrams |
| `bgp_bfd_unknown_peer_total` | Counter | From an address with no configured peer. **Typically a link-local neighbour configured without its `%zone`** — transmit works, BGP looks healthy, BFD never leaves DOWN. Global counter with no peer label, so diagnosis needs daemon debug logs |
| `bgp_bfd_wrong_hop_limit_total` | Counter | TTL/hop limit was not 255, which RFC 5881 §5 requires |

### Netlink (kernel FIB)

Import: `bgp_netlink_import_enabled`, `bgp_netlink_imported_total`,
`bgp_netlink_import_withdrawn_total`, `bgp_netlink_import_errors_total`,
`bgp_netlink_import_loop_ticks_total`, `bgp_netlink_import_addr_events_total`.

Export: `bgp_netlink_export_enabled`, `bgp_netlink_exported_total`,
`bgp_netlink_export_withdrawn_total`, `bgp_netlink_export_errors_total`,
`bgp_netlink_nexthop_validation_total`, `bgp_netlink_nexthop_failed_total`,
`bgp_netlink_dampened_updates_total`, `bgp_netlink_cleanup_routes_deleted_total`,
`bgp_netlink_cleanup_routes_skipped_total`.

All unlabelled. Two are easy to confuse:

- **`import_loop_ticks_total`** counts scan iterations and stops advancing when the loop exits.
  Flat while import is enabled means **the importer has stopped** — this is the liveness signal.
- **`import_addr_events_total`** counts kernel address changes. It is legitimately flat for hours
  on a quiet cluster. Alerting on it flat is a guaranteed false positive; it is useful only
  *relative* to address churn you know is happening.

### Build identity

`bgp_build_info{version, commit, base}` — always 1, read the labels. An empty `commit` means the
binary was not built with the version ldflag.

### Loop timing histograms

13 families, each with `ExponentialBuckets` spanning 10µs to 10s. Every timed operation splits
into three non-overlapping intervals that sum to end-to-end latency:

| Suffix | Meaning |
|---|---|
| `_queue_wait_seconds` | Queued, between creation and receipt |
| `_lock_wait_seconds` | Blocked acquiring the BGP lock |
| `_work_seconds` | Holding the lock, doing the work |

| Family | Has queue wait? |
|---|---|
| `fsm_loop_mgmt_op_*` | yes |
| `fsm_loop_accept_*` | yes |
| `fsm_loop_roa_event_*` | yes |
| `bgp_message_handling_*` | no |
| `bgp_state_change_handling_*` | no |

**Note the namespaces.** `message_handling` and `state_change_handling` are under `bgp_`, *not*
`fsm_loop_`, because they run on per-peer goroutines rather than the Serve loop. A relabel rule
written as `fsm_loop_.*` misses them; one written as `bgp_.*` catches four histogram families you
may not have intended.

`bgp_message_handling_lock_wait_seconds` rising with flat `_work_seconds` means peers are waiting
on the BGP lock rather than doing slow work — the contended resource is the lock, not any one
goroutine.

---

## Removed metrics

These exist in k8gobgp today and are removed once gobgp-netlink emits them natively.

| Removed | Replacement |
|---|---|
| `k8gobgp_neighbors{state}` | `count by (instance, job, session_state) (bgp_peer_state)` |
| `k8gobgp_neighbor_state{neighbor,state}` | `bgp_peer_state{peer, session_state, admin_state}` |
| `k8gobgp_neighbor_session_flaps_total{neighbor}` | `delta(bgp_peer_flop_count[15m])` — **gauge, so `delta` not `increase`** |
| `k8gobgp_neighbor_session_established_timestamp_seconds{neighbor}` | `bgp_peer_established_timestamp_seconds` **gated on state** — see below |
| `k8gobgp_neighbor_routes_received{neighbor,family}` | `bgp_routes_received{peer, route_family}` |
| `k8gobgp_neighbor_routes_accepted{neighbor,family}` | `bgp_routes_accepted{peer, route_family}` |
| `k8gobgp_neighbor_routes_advertised{neighbor,family}` | `bgp_routes_advertised{peer, route_family}` |
| `k8gobgp_routes_received` | `sum by (instance, job) (bgp_routes_received)` |
| `k8gobgp_routes_accepted` | `sum by (instance, job) (bgp_routes_accepted)` |
| `k8gobgp_routes_advertised` | `sum by (instance, job) (bgp_routes_advertised)` |
| `k8gobgp_neighbor_metrics_truncated` | none — existed only for the removed cardinality limiter |
| `k8gobgp_metrics_cardinality_limit_hit_total` | none — same |
| `k8gobgp_{neighbors,peer_groups,dynamic_neighbors,vrfs,policies,defined_sets}_configured` | `k8gobgp_configured_objects{kind="..."}` |
| `k8gobgp_router_id_source{source}` | `count by (source) (k8gobgp_router_id_info)` |
| `k8gobgp_metrics_collection_skipped_total` | none — unreachable in practice |
| `k8gobgp_nodestatus_last_successful_write_timestamp` | renamed `..._timestamp_seconds` |

### The established-timestamp difference

k8gobgp's version was **absent** while a session was down. The fork's is **retained** — it holds
the last establishment time after the session drops. So a peer down for an hour looks identical
to a peer up for an hour if you read it alone. Gate on state:

```promql
bgp_peer_established_timestamp_seconds
  and on(instance, job, peer) bgp_peer_state{session_state="SESSION_STATE_ESTABLISHED"}
```

Both are absolute Unix timestamps, not durations. Session age is `time() - <metric>`, but only
for peers that pass the gate above — an ungated `time() -` over a never-established peer yields
roughly 56 years.

---

## Label traps

These are not mechanical renames and a naive find-and-replace will produce queries that silently
return nothing.

| | k8gobgp | gobgp-netlink |
|---|---|---|
| Peer identity label | `neighbor` | `peer` |
| Session state label | `state` | `session_state` |
| Session state value | `established` | `SESSION_STATE_ESTABLISHED` |
| Address family label | `family` | `route_family` |
| Address family value | `ipv4_unicast` (underscore) | `ipv4-unicast` (**hyphen**) |
| Unnumbered peer identity | `iface:eth0` | `fe80::a1b2%eth0` |

Three consequences worth stating outright:

1. **`k8gobgp_rib_routes{family="ipv4_unicast"}` and `bgp_routes_*{route_family="ipv4-unicast"}`
   cannot be joined** without a relabel. The label name differs *and* the separator differs.
2. **Unnumbered peer identity is now per-node.** `iface:eth0` was identical on every node, so
   `by (neighbor)` aggregated across the fleet. `fe80::a1b2%eth0` is the resolved link-local
   address, which differs per node — fleet-wide aggregation by peer no longer means anything.
3. **Unnumbered peer series reset when the remote's address changes.** A peer reboot with a new
   EUI-64 mints a new series, so `delta()`/`increase()` over `bgp_peer_flop_count` or
   `bgp_peer_bfd_failure_transitions_total` treats it as a counter reset.

Session state enum values are the protobuf names: `SESSION_STATE_UNSPECIFIED`, `_IDLE`,
`_CONNECT`, `_ACTIVE`, `_OPENSENT`, `_OPENCONFIRM`, `_ESTABLISHED`. BFD states are
`BFD_SESSION_STATE_UNSPECIFIED`, `_UP`, `_DOWN`, `_ADMIN_DOWN`, `_INIT` — note the numbering does
not follow RFC 5880 §6.8.1, so read the name, not the value.

---

## Scrape configuration

Two endpoints means two scrape targets, and every cross-endpoint query below depends on them
sharing a per-node label. `docs/monitoring/podmonitors.yaml` has Prometheus Operator PodMonitors
that set `job` explicitly per endpoint and stamp `node` from the pod's node name;
`docs/alerting/k8gobgp-alerts.yaml` has rules that assume exactly that shape.

## Common queries

```promql
# Nodes with neighbours configured but none established.
# count() over a non-matching selector returns an EMPTY VECTOR, not 0 — so `== 0` never fires.
count by (instance, job) (bgp_peer_state) > 0
  unless
count by (instance, job) (bgp_peer_state{session_state="SESSION_STATE_ESTABLISHED"}) > 0

# The CR asked for more neighbours than gobgpd has. Catches a peer gobgpd refused,
# which no bgp_* metric can describe because it never enters ListPeer.
#
# Grouped by `node`, and that is not a style choice. This is the only query here
# that compares a k8gobgp metric against a gobgpd one, and the two are separate
# scrape targets - :7473 and :7475 - so `instance` is host:7473 on one side and
# host:7475 on the other, and `job` differs by construction. A binary operator
# between two vectors with disjoint labels returns nothing, so grouping by either
# makes this silent rather than wrong. Neither metric carries a node label of its
# own; docs/monitoring/podmonitors.yaml stamps one from the pod's node name.
#
# max, not sum: with two BGPConfigurations visible on a node only one owns gobgpd,
# and summing both overstates the expected count.
max by (node) (k8gobgp_configured_objects{kind="neighbor"})
  > count by (node) (bgp_peer_state)

# Flapping peers.
delta(bgp_peer_flop_count[15m]) > 2

# Netlink import has stopped. Use loop_ticks, NOT addr_events.
rate(bgp_netlink_import_loop_ticks_total[10m]) == 0 and bgp_netlink_import_enabled == 1

# Routes failing to reach the kernel FIB.
rate(bgp_netlink_export_errors_total[5m]) > 0
rate(bgp_netlink_nexthop_failed_total[5m]) > 0

# BFD configured somewhere on this node but the socket is not bound.
bgp_bfd_server_up == 0
  and on(instance, job) (max by (instance, job) (bgp_peer_bfd_enabled) == 1)

# Link-local peer configured without its zone.
rate(bgp_bfd_unknown_peer_total[10m]) > 0

# Build skew across the fleet. Needs a long `for:` — it is true during every rollout.
count(count by (version, commit, base) (bgp_build_info)) > 1

# Stale node status.
time() - k8gobgp_nodestatus_last_successful_write_timestamp_seconds > 300
```

Scope per-peer alerts with `on(instance, job, peer)`. A bare `on(peer)` lets one healthy node
suppress the alert for every other node, because every node peers with the same ToR addresses.

---

## Cardinality

gobgp-netlink emits, per peer, where `F` is the number of enabled address families:

| | Series per peer |
|---|---|
| Without BFD | **29 + 3F** |
| With BFD | **33 + 3F** |

At `F=2` that is 35 and 39. Fixed per-node cost is roughly 23 series (netlink 15, BFD server 7,
build info 1) plus 13 histogram families and the Go/process collectors.

**There is no in-process limit on peer count.** If you run many peers per node — dynamic
neighbours in particular — filter at scrape time. Apply the keep-list to the **7475 endpoint
only**; applying it to 7473 would delete the controller-runtime and workqueue metrics you need to
diagnose the controller.

```yaml
metricRelabelConfigs:
  - sourceLabels: [__name__]
    action: keep
    regex: '(bgp_peer_(state|type|asn|local_asn|established_timestamp_seconds|flop_count|out_queue_count|password_set|bfd_.*)|bgp_(received|sent)_notification_total|bgp_routes_.*|bgp_(bfd|netlink)_.*|bgp_build_info|bgp_message_handling_.*|fsm_loop_mgmt_op_lock_wait_seconds.*|go_(goroutines|threads|memstats_alloc_bytes)|process_(cpu_seconds_total|resident_memory_bytes|open_fds))'
```

That drops the 18 message counters except NOTIFICATION, and 11 of the 13 histogram families,
taking a peer from ~35 series to ~14. Keep `go_*` and `process_*`: when
`bgp_bfd_received_drop_total` advances it means an event loop stalled, and with pprof disabled
those are the only runtime signals left.

---

## What is deliberately not a metric

**BFD sub-second state.** The daemon's BGP collector is cached (`--metrics-min-interval`, default
15s) and then scraped on your interval, so per-peer BFD state can be ~45s stale. BFD detects
failure in 900ms–3s. Metrics will tell you a session *has been* flapping; they will not tell you
its state right now. For ground truth use `kubectl get bgpnodestatus <node>` or
`gobgp neighbor <addr>`.

**Session state in health probes.** `/readyz` reports whether both processes are running, not
whether BGP is up. Zero peers is a valid steady state — a deployment using local-address mode runs
the daemon with no peering at all — so a probe that failed on session state would mark those nodes
NotReady permanently. Session state belongs in alerts, not in scheduling decisions.
