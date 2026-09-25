// Copyright 2025 Acnodal Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metrics for the BGPConfiguration controller
var (
	// Reconcile metrics
	reconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_reconcile_total",
			Help: "Total number of reconciliations per BGPConfiguration",
		},
		[]string{"name", "namespace", "result"},
	)

	reconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "k8gobgp_reconcile_duration_seconds",
			Help:    "Duration of reconciliation in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 15), // 1ms to ~16s
		},
		[]string{"name", "namespace"},
	)

	// What the CR asks for on this node, after nodeSelector filtering. One
	// gauge with a kind label, replacing six that differed only in subject:
	// k8gobgp_{neighbors,peer_groups,dynamic_neighbors,vrfs,policies,defined_sets}_configured.
	//
	// This has no equivalent in gobgp-netlink's metrics, which are all derived
	// from what the daemon actually holds. Comparing the two is how you see a
	// neighbor the daemon refused:
	//
	//   sum by (instance, job) (k8gobgp_configured_objects{kind="neighbor"})
	//     > count by (instance, job) (bgp_peer_state)
	bgpConfiguredObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_configured_objects",
			Help: "Objects this BGPConfiguration asks for on this node, by kind",
		},
		[]string{"kind", "name", "namespace"},
	)

	// GoBGP connection metrics
	gobgpConnectionStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_gobgpd_connection_status",
			Help: "GoBGP daemon connection status (1=connected, 0=disconnected)",
		},
		[]string{"endpoint"},
	)

	gobgpConnectionErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_gobgpd_connection_errors_total",
			Help: "Total number of GoBGP connection errors",
		},
		[]string{"endpoint"},
	)

	// Configuration metrics
	bgpConfigurationReady = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_configuration_ready",
			Help: "BGPConfiguration ready status (1=ready, 0=not ready)",
		},
		[]string{"name", "namespace"},
	)

	// Cleanup metrics
	cleanupRetries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_cleanup_retries_total",
			Help: "Total number of cleanup retries during deletion",
		},
		[]string{"name", "namespace"},
	)

	cleanupDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "k8gobgp_cleanup_duration_seconds",
			Help:    "Duration of cleanup operations in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms to ~5s
		},
		[]string{"name", "namespace"},
	)

	// === BGP Stats Metrics (collected by BGPMetricsController) ===

	// Global RIB route counts by address family
	bgpRibRoutes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_rib_routes",
			Help: "Number of route prefixes in the global RIB by address family",
		},
		[]string{"family"}, // family: ipv4_unicast, ipv6_unicast, l2vpn_evpn
	)

	// The per-neighbor and aggregate BGP stats that used to live here are gone:
	// gobgp-netlink emits all of them natively, from the daemon that owns the
	// data, so re-exporting them over gRPC was duplication. See docs/metrics.md
	// for the mapping - the labels are not a straight rename.
	//
	//   k8gobgp_neighbors{state}                     -> count by (session_state) (bgp_peer_state)
	//   k8gobgp_neighbor_state{neighbor,state}       -> bgp_peer_state{peer,session_state,admin_state}
	//   k8gobgp_neighbor_session_flaps_total         -> delta(bgp_peer_flop_count[...])  (it is a gauge)
	//   k8gobgp_neighbor_session_established_...     -> bgp_peer_established_timestamp_seconds, gated on state
	//   k8gobgp_neighbor_routes_{received,...}       -> bgp_routes_*{peer,route_family}
	//   k8gobgp_routes_{received,accepted,advertised} -> sum(bgp_routes_*)
	//
	// k8gobgp_rib_routes above stays: every fork collector is per-peer and none
	// calls GetTable, so global RIB size has no native equivalent.

	// Metrics collection health metrics
	metricsCollectionDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "k8gobgp_metrics_collection_duration_seconds",
			Help:    "Time taken to collect BGP metrics from gobgpd",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30},
		},
	)

	metricsCollectionErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "k8gobgp_metrics_collection_errors_total",
			Help: "Total errors during BGP metrics collection",
		},
	)

	// k8gobgp_metrics_collection_skipped_total and
	// k8gobgp_metrics_cardinality_limit_hit_total are removed. The first counted
	// "previous collection still running", which the 10s collection timeout
	// against a 15s poll floor already made unreachable, and the slimmed loop
	// makes more so. The second existed only for the per-neighbor cardinality
	// limiter, which went with the per-neighbor metrics.

	// === Router ID Resolution Metrics ===

	// Router ID resolution counter
	routerIDResolutionTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_router_id_resolution_total",
			Help: "Total number of router ID resolution attempts",
		},
		[]string{"result"}, // result: success, failure
	)

	// Router ID resolution duration
	routerIDResolutionDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "k8gobgp_router_id_resolution_duration_seconds",
			Help:    "Duration of router ID resolution in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms to ~2s
		},
	)

	// k8gobgp_router_id_source is removed: k8gobgp_router_id_info already carries
	// a source label, so count by (source) (k8gobgp_router_id_info) is the same
	// answer from one metric instead of two.

	// Resolved router ID info (provides the actual value for observability)
	routerIDInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_router_id_info",
			Help: "Router ID information (value is always 1, labels provide details)",
		},
		[]string{"router_id", "source", "node", "asn", "name", "namespace"},
	)

	// globalRestartRequired is 1 for each global setting whose value in the CR
	// differs from what gobgpd is running.
	//
	// A gauge rather than an event because the event is reconcile-gated and
	// ephemeral: Reconcile fires on spec generation changes, so a drift that
	// nobody edits again produces one event that then ages out, while the daemon
	// keeps running the old value until the pod restarts. The gauge stays up for
	// as long as the drift does.
	//
	// The field label is bounded to the known Global field names - it is derived
	// from a fixed comparison list, never from CR content - so this cannot grow
	// unboundedly.
	globalRestartRequired = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_global_restart_required",
			Help: "1 when a global setting in the CR differs from the running gobgpd and needs a pod restart to apply",
		},
		[]string{"field", "name", "namespace"},
	)

	// === BGPNodeStatus Reporter Metrics ===

	nodeStatusWriteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_nodestatus_write_total",
			Help: "Total number of BGPNodeStatus write attempts",
		},
		[]string{"result"}, // success, error, skipped
	)

	nodeStatusCollectionDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "k8gobgp_nodestatus_collection_duration_seconds",
			Help:    "Time taken to collect node status data from gobgpd and netlink",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30},
		},
	)

	nodeStatusLastSuccessfulWrite = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_nodestatus_last_successful_write_timestamp_seconds",
			Help: "Unix timestamp of the last successful BGPNodeStatus write",
		},
	)

	nodeStatusObjectSizeBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_nodestatus_object_size_bytes",
			Help: "Approximate size of the BGPNodeStatus object in bytes",
		},
	)

	// AddPeer/UpdatePeer failures, by neighbor key and operation.
	//
	// This is the only signal for a peer gobgpd refused. A rejected neighbor
	// never enters gobgpd's neighborMap, so it never appears in ListPeer and no
	// bgp_* metric describes it - it is simply absent, which is indistinguishable
	// from never having been configured. The reconciler used to swallow these
	// errors into a log line.
	peerApplyErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_peer_apply_errors_total",
			Help: "Failures applying a peer or peer group to gobgpd, by key and operation",
		},
		[]string{"key", "op"},
	)
)

func init() {
	// Register all metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDuration,
		bgpConfiguredObjects,
		gobgpConnectionStatus,
		gobgpConnectionErrors,
		bgpConfigurationReady,
		peerApplyErrors,
		cleanupRetries,
		cleanupDuration,
		// Global RIB size, polled by BGPMetricsController. No native equivalent.
		bgpRibRoutes,
		metricsCollectionDuration,
		metricsCollectionErrors,
		// Router ID resolution metrics
		routerIDResolutionTotal,
		routerIDResolutionDuration,
		routerIDInfo,
		globalRestartRequired,
		// BGPNodeStatus reporter metrics
		nodeStatusWriteTotal,
		nodeStatusCollectionDuration,
		nodeStatusLastSuccessfulWrite,
		nodeStatusObjectSizeBytes,
	)
}

// RecordReconcileResult records the result of a reconciliation
func RecordReconcileResult(name, namespace, result string) {
	reconcileTotal.WithLabelValues(name, namespace, result).Inc()
}

// RecordReconcileDuration records the duration of a reconciliation
func RecordReconcileDuration(name, namespace string, duration float64) {
	reconcileDuration.WithLabelValues(name, namespace).Observe(duration)
}

// Object kinds for k8gobgp_configured_objects. These are the label values every
// query depends on, so they are constants rather than inline strings.
const (
	KindNeighbor        = "neighbor"
	KindPeerGroup       = "peer_group"
	KindDynamicNeighbor = "dynamic_neighbor"
	KindVrf             = "vrf"
	KindPolicy          = "policy"
	KindDefinedSet      = "defined_set"
)

// UpdateConfiguredObjects records how many objects of a kind this CR asks for on
// this node, after nodeSelector filtering. Written by the reconciler: it
// describes intent, so it is legitimately edge-triggered.
func UpdateConfiguredObjects(kind, name, namespace string, count int) {
	bgpConfiguredObjects.WithLabelValues(kind, name, namespace).Set(float64(count))
}

// RecordGoBGPConnection records whether gobgpd answered its last RPC. Owned by
// the gobgpd readiness check - see GoBGPDChecker.
func RecordGoBGPConnection(endpoint string, connected bool) {
	if connected {
		gobgpConnectionStatus.WithLabelValues(endpoint).Set(1)
	} else {
		gobgpConnectionStatus.WithLabelValues(endpoint).Set(0)
	}
}

// RecordGoBGPConnectionError counts failed attempts to reach gobgpd.
func RecordGoBGPConnectionError(endpoint string) {
	gobgpConnectionErrors.WithLabelValues(endpoint).Inc()
}

// RecordPeerApplyError records a failure applying a peer or peer group. See the
// metric's declaration: a peer gobgpd refused is otherwise invisible.
func RecordPeerApplyError(key, op string) {
	peerApplyErrors.WithLabelValues(key, op).Inc()
}

// UpdateConfigurationReadyStatus updates the ready status of a configuration
func UpdateConfigurationReadyStatus(name, namespace string, ready bool) {
	if ready {
		bgpConfigurationReady.WithLabelValues(name, namespace).Set(1)
	} else {
		bgpConfigurationReady.WithLabelValues(name, namespace).Set(0)
	}
}

// RecordCleanupRetry records a cleanup retry
func RecordCleanupRetry(name, namespace string) {
	cleanupRetries.WithLabelValues(name, namespace).Inc()
}

// RecordCleanupDuration records the duration of a cleanup operation
func RecordCleanupDuration(name, namespace string, duration float64) {
	cleanupDuration.WithLabelValues(name, namespace).Observe(duration)
}

// The FSM state label vocabulary, fsmStateLabel, AllFSMStates, NeighborSample
// and SetNeighborMetrics are gone with the per-neighbor metrics they served.
// gobgp-netlink labels bgp_peer_state with the protobuf enum name
// ("SESSION_STATE_ESTABLISHED"), not this package's lowercase form.
//
// peerStateToString in bgpnodestatus_reporter.go is unaffected: it renders the
// CR's .status.neighbors[].state ("Established"), which is a third vocabulary
// and stays as it is.

// DeleteMetricsForConfig removes all metrics for a deleted configuration.
//
// Without this a deleted CR's gauges keep their last value forever, so a
// configuration that no longer exists still reads as configured and ready.
func DeleteMetricsForConfig(name, namespace string) {
	for _, kind := range []string{
		KindNeighbor, KindPeerGroup, KindDynamicNeighbor,
		KindVrf, KindPolicy, KindDefinedSet,
	} {
		bgpConfiguredObjects.DeleteLabelValues(kind, name, namespace)
	}
	bgpConfigurationReady.DeleteLabelValues(name, namespace)
}

// RecordRouterIDResolution records the result of a router ID resolution attempt
func RecordRouterIDResolution(result string, duration float64) {
	routerIDResolutionTotal.WithLabelValues(result).Inc()
	routerIDResolutionDuration.Observe(duration)
}

// UpdateRouterIDInfo updates the router ID information metric
// UpdateRouterIDInfo sets this CR's router ID series, first removing its
// previous one if the resolved values changed.
//
// Deliberately not Reset(): that wiped the entire vector on every call, so with
// two BGPConfigurations on one node the second CR's resolution erased the
// first's series. Labels are passed as maps rather than positionally — the
// vector carries six labels now, and a future reordering would silently
// mismatch a positional delete against the wrong series.
func UpdateRouterIDInfo(oldLabels, newLabels prometheus.Labels) {
	if oldLabels != nil {
		routerIDInfo.Delete(oldLabels)
	}
	routerIDInfo.With(newLabels).Set(1)
}

// SetGlobalRestartRequired records which global settings have drifted, and
// clears the ones that have not.
//
// Every field in the comparison is passed every time, drifted or not, so a
// setting that stops drifting has its series set back to 0 rather than left at 1
// for ever. Setting 0 rather than deleting keeps the series present, which is
// what lets an alert use a plain `> 0` without worrying about absent series.
func SetGlobalRestartRequired(name, namespace string, drifted map[string]bool) {
	for field, isDrifted := range drifted {
		v := 0.0
		if isDrifted {
			v = 1.0
		}
		globalRestartRequired.WithLabelValues(field, name, namespace).Set(v)
	}
}

// DeleteGlobalRestartRequired removes one CR's drift series. Required on CR
// deletion, for the same reason as DeleteRouterIDInfo: nothing else clears them.
func DeleteGlobalRestartRequired(name, namespace string, fields []string) {
	for _, f := range fields {
		globalRestartRequired.DeleteLabelValues(f, name, namespace)
	}
}

// DeleteRouterIDInfo removes one CR's router ID series. Required on CR
// deletion: without Reset() there is nothing else to clear it, so the series
// would otherwise outlive the CR indefinitely.
func DeleteRouterIDInfo(labels prometheus.Labels) {
	if labels != nil {
		routerIDInfo.Delete(labels)
	}
}
