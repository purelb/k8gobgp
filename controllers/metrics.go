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
	gobgpapi "github.com/osrg/gobgp/v4/api"
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

	// BGP session metrics
	bgpNeighborsConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbors_configured",
			Help: "Number of BGP neighbors configured",
		},
		[]string{"name", "namespace"},
	)

	bgpPeerGroupsConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_peer_groups_configured",
			Help: "Number of BGP peer groups configured",
		},
		[]string{"name", "namespace"},
	)

	bgpDynamicNeighborsConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_dynamic_neighbors_configured",
			Help: "Number of BGP dynamic neighbors configured",
		},
		[]string{"name", "namespace"},
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

	// VRF metrics
	bgpVrfsConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_vrfs_configured",
			Help: "Number of VRFs configured",
		},
		[]string{"name", "namespace"},
	)

	// Policy metrics
	bgpPoliciesConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_policies_configured",
			Help: "Number of BGP policies configured",
		},
		[]string{"name", "namespace"},
	)

	bgpDefinedSetsConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_defined_sets_configured",
			Help: "Number of defined sets configured",
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

	// Node-level neighbor counts by BGP FSM state (from periodic polling).
	// One series per state, always all of them, so a state with no peers reads 0
	// rather than being absent. Replaces the former neighbors_total /
	// neighbors_established_total / neighbors_active / neighbors_idle gauges,
	// which between them covered only three of the seven FSM states — a peer
	// mid-handshake was counted nowhere and the states did not sum to the total.
	// sum without(state) gives the total.
	bgpNeighbors = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbors",
			Help: "Number of BGP neighbors on this node by FSM session state",
		},
		[]string{"state"},
	)

	// Per-neighbor FSM state as an info metric: exactly one series per peer,
	// carrying its current state as a label, value always 1. Costs one series
	// per peer rather than the seven a state-set would, which is what makes it
	// affordable to enable by default.
	bgpNeighborState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbor_state",
			Help: "Current BGP FSM state of each neighbor (value is always 1; the state label carries the value). Subject to --max-neighbors-metrics; see k8gobgp_neighbor_metrics_truncated",
		},
		[]string{"neighbor", "state"},
	)

	// Session flap count, taken from gobgpd's own PeerState.Flops. A real
	// counter survives pod restarts and catches flaps that complete between two
	// polls — both of which changes() over a timestamp misses.
	bgpNeighborSessionFlaps = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "k8gobgp_neighbor_session_flaps_total",
			Help: "Number of times each neighbor's BGP session has flapped, as counted by gobgpd",
		},
		[]string{"neighbor"},
	)

	// Absent, not zero, while a session is down: zero is a plausible-looking
	// timestamp and would make time() - <this> yield ~1.7e9 for every down peer,
	// silently poisoning any session-age query.
	bgpNeighborSessionEstablished = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbor_session_established_timestamp_seconds",
			Help: "Unix timestamp at which each neighbor's session reached ESTABLISHED. Absent while the session is down",
		},
		[]string{"neighbor"},
	)

	// Without this, peers beyond the cap are a silent monitoring blind spot:
	// the node-level counts include them but no per-neighbor series exists, so
	// "some peer is down" alerts cannot fire for them.
	bgpNeighborMetricsTruncated = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbor_metrics_truncated",
			Help: "Number of neighbors omitted from per-neighbor metrics because of --max-neighbors-metrics",
		},
	)

	// Per-neighbor route stats (high cardinality - opt-in)
	bgpNeighborRoutesReceived = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbor_routes_received",
			Help: "Number of routes received from neighbor by address family (opt-in, high cardinality)",
		},
		[]string{"neighbor", "family"},
	)

	bgpNeighborRoutesAccepted = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbor_routes_accepted",
			Help: "Number of routes accepted from neighbor by address family (opt-in, high cardinality)",
		},
		[]string{"neighbor", "family"},
	)

	bgpNeighborRoutesAdvertised = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbor_routes_advertised",
			Help: "Number of routes advertised to neighbor by address family (opt-in, high cardinality)",
		},
		[]string{"neighbor", "family"},
	)

	// Aggregate route counts (low cardinality alternative)
	bgpRoutesReceivedTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_routes_received",
			Help: "Total routes received from all neighbors (sum across all neighbors and families)",
		},
	)

	bgpRoutesAcceptedTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_routes_accepted",
			Help: "Total routes accepted from all neighbors",
		},
	)

	bgpRoutesAdvertisedTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_routes_advertised",
			Help: "Total routes advertised to all neighbors",
		},
	)

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

	metricsCollectionSkipped = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "k8gobgp_metrics_collection_skipped_total",
			Help: "Collections skipped due to previous collection still running",
		},
	)

	metricsCardinalityLimitHit = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "k8gobgp_metrics_cardinality_limit_hit_total",
			Help: "Number of times per-neighbor metrics cardinality limit was hit",
		},
	)

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

	// Router ID source (shows the method used to determine router ID)
	routerIDSource = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_router_id_source",
			Help: "Router ID resolution source (1 = active source)",
		},
		[]string{"source"}, // source: explicit, template, node-ipv4, hash-from-node-name
	)

	// Resolved router ID info (provides the actual value for observability)
	routerIDInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_router_id_info",
			Help: "Router ID information (value is always 1, labels provide details)",
		},
		[]string{"router_id", "source", "node", "asn", "name", "namespace"},
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
			Name: "k8gobgp_nodestatus_last_successful_write_timestamp",
			Help: "Unix timestamp of the last successful BGPNodeStatus write",
		},
	)

	nodeStatusObjectSizeBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_nodestatus_object_size_bytes",
			Help: "Approximate size of the BGPNodeStatus object in bytes",
		},
	)
)

func init() {
	// Register all metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDuration,
		bgpNeighborsConfigured,
		bgpPeerGroupsConfigured,
		bgpDynamicNeighborsConfigured,
		gobgpConnectionStatus,
		gobgpConnectionErrors,
		bgpConfigurationReady,
		bgpVrfsConfigured,
		bgpPoliciesConfigured,
		bgpDefinedSetsConfigured,
		cleanupRetries,
		cleanupDuration,
		// BGP Stats Metrics (collected by BGPMetricsController)
		bgpRibRoutes,
		bgpNeighbors,
		bgpNeighborState,
		bgpNeighborSessionFlaps,
		bgpNeighborSessionEstablished,
		bgpNeighborMetricsTruncated,
		bgpNeighborRoutesReceived,
		bgpNeighborRoutesAccepted,
		bgpNeighborRoutesAdvertised,
		bgpRoutesReceivedTotal,
		bgpRoutesAcceptedTotal,
		bgpRoutesAdvertisedTotal,
		metricsCollectionDuration,
		metricsCollectionErrors,
		metricsCollectionSkipped,
		metricsCardinalityLimitHit,
		// Router ID resolution metrics
		routerIDResolutionTotal,
		routerIDResolutionDuration,
		routerIDSource,
		routerIDInfo,
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

// BGP FSM state label values.
//
// Declared here rather than derived from peerStateToString: that function's
// output is the BGPNodeStatus CRD's documented .status.neighbors[].state
// contract, and wiring metric labels to it would make a CRD casing change
// silently rewrite every metric label — breaking all alerts with no compile
// error. TestFSMStateLabelsMatchCRDStates keeps the two sets in step by
// failing loudly instead.
const (
	FSMStateIdle        = "idle"
	FSMStateConnect     = "connect"
	FSMStateActive      = "active"
	FSMStateOpenSent    = "opensent"
	FSMStateOpenConfirm = "openconfirm"
	FSMStateEstablished = "established"
	FSMStateUnknown     = "unknown"
)

// AllFSMStates is every value the state label can take. The node-level gauge is
// pre-initialized across all of them so an absent state reads 0 rather than
// disappearing — a missing series and a zero one mean very different things to
// an alert.
var AllFSMStates = []string{
	FSMStateIdle, FSMStateConnect, FSMStateActive, FSMStateOpenSent,
	FSMStateOpenConfirm, FSMStateEstablished, FSMStateUnknown,
}

// fsmStateLabel maps a gobgp session state to its metric label value.
// SESSION_STATE_UNSPECIFIED and any state a future gobgp adds fall through to
// "unknown" rather than vanishing — the previous switch covered only three of
// the seven states, so a peer mid-handshake was counted nowhere at all.
func fsmStateLabel(state gobgpapi.PeerState_SessionState) string {
	switch state {
	case gobgpapi.PeerState_SESSION_STATE_IDLE:
		return FSMStateIdle
	case gobgpapi.PeerState_SESSION_STATE_CONNECT:
		return FSMStateConnect
	case gobgpapi.PeerState_SESSION_STATE_ACTIVE:
		return FSMStateActive
	case gobgpapi.PeerState_SESSION_STATE_OPENSENT:
		return FSMStateOpenSent
	case gobgpapi.PeerState_SESSION_STATE_OPENCONFIRM:
		return FSMStateOpenConfirm
	case gobgpapi.PeerState_SESSION_STATE_ESTABLISHED:
		return FSMStateEstablished
	default:
		return FSMStateUnknown
	}
}

// UpdateNeighborsConfigured updates the count of neighbors this CR asks for on
// this node, after nodeSelector filtering. Written by the reconciler: it
// describes intent, so it is legitimately edge-triggered.
func UpdateNeighborsConfigured(name, namespace string, count int) {
	bgpNeighborsConfigured.WithLabelValues(name, namespace).Set(float64(count))
}

// SetNeighborStateCounts replaces the node-level per-state counts. Every state
// in AllFSMStates is written, including zeros, so the series never vanish.
func SetNeighborStateCounts(counts map[string]int) {
	for _, state := range AllFSMStates {
		bgpNeighbors.WithLabelValues(state).Set(float64(counts[state]))
	}
}

// UpdatePeerGroupMetrics updates the peer group count
func UpdatePeerGroupMetrics(name, namespace string, count int) {
	bgpPeerGroupsConfigured.WithLabelValues(name, namespace).Set(float64(count))
}

// UpdateDynamicNeighborMetrics updates the dynamic neighbor count
func UpdateDynamicNeighborMetrics(name, namespace string, count int) {
	bgpDynamicNeighborsConfigured.WithLabelValues(name, namespace).Set(float64(count))
}

// RecordGoBGPConnection records the GoBGP connection status
func RecordGoBGPConnection(endpoint string, connected bool) {
	if connected {
		gobgpConnectionStatus.WithLabelValues(endpoint).Set(1)
	} else {
		gobgpConnectionStatus.WithLabelValues(endpoint).Set(0)
	}
}

// RecordGoBGPConnectionError records a GoBGP connection error
func RecordGoBGPConnectionError(endpoint string) {
	gobgpConnectionErrors.WithLabelValues(endpoint).Inc()
}

// UpdateConfigurationReadyStatus updates the ready status of a configuration
func UpdateConfigurationReadyStatus(name, namespace string, ready bool) {
	if ready {
		bgpConfigurationReady.WithLabelValues(name, namespace).Set(1)
	} else {
		bgpConfigurationReady.WithLabelValues(name, namespace).Set(0)
	}
}

// UpdateVrfMetrics updates the VRF count
func UpdateVrfMetrics(name, namespace string, count int) {
	bgpVrfsConfigured.WithLabelValues(name, namespace).Set(float64(count))
}

// UpdatePolicyMetrics updates the policy and defined sets counts
func UpdatePolicyMetrics(name, namespace string, policies, definedSets int) {
	bgpPoliciesConfigured.WithLabelValues(name, namespace).Set(float64(policies))
	bgpDefinedSetsConfigured.WithLabelValues(name, namespace).Set(float64(definedSets))
}

// RecordCleanupRetry records a cleanup retry
func RecordCleanupRetry(name, namespace string) {
	cleanupRetries.WithLabelValues(name, namespace).Inc()
}

// RecordCleanupDuration records the duration of a cleanup operation
func RecordCleanupDuration(name, namespace string, duration float64) {
	cleanupDuration.WithLabelValues(name, namespace).Observe(duration)
}

// NeighborSample is one peer's observation from a single collection pass.
type NeighborSample struct {
	Key                  string // neighborKey(): an address, or "iface:<name>"
	State                string // one of AllFSMStates
	Flaps                uint32 // gobgpd's PeerState.Flops
	EstablishedTimestamp int64  // unix seconds; 0 when not established
}

// SetNeighborMetrics replaces the per-neighbor series for one collection pass,
// then removes the series of any peer seen last time but not this time.
//
// Deliberately not a Reset(): the vectors are exported by default, and Reset
// empties them for the whole duration of a collection — up to the 10s timeout.
// A scrape landing in that window would see the metric absent, which "== 0" and
// absent() alerts read as "the peer is gone". Deleting only what actually
// departed leaves no gap.
//
// truncated is the number of peers omitted by the cardinality cap, exported so
// the blind spot is visible rather than silent.
func SetNeighborMetrics(samples []NeighborSample, truncated int, prev map[string]NeighborSample) map[string]NeighborSample {
	seen := make(map[string]NeighborSample, len(samples))
	for _, s := range samples {
		seen[s.Key] = s
		p, existed := prev[s.Key]

		// One series per peer, so the previous state label must go before the
		// current one is set — otherwise a peer that moves idle -> established
		// leaves both series exported at 1 forever.
		if existed && p.State != s.State {
			bgpNeighborState.DeletePartialMatch(prometheus.Labels{"neighbor": s.Key})
		}
		bgpNeighborState.WithLabelValues(s.Key, s.State).Set(1)

		// gobgpd reports Flops as an absolute count; a Prometheus counter only
		// accepts increments. Add the delta, and if the count goes backwards
		// (gobgpd restarted and began again from zero) adopt the new absolute
		// value rather than going negative.
		switch {
		case !existed, s.Flaps < p.Flaps:
			if s.Flaps > 0 {
				bgpNeighborSessionFlaps.WithLabelValues(s.Key).Add(float64(s.Flaps))
			} else {
				// Touch the series so a peer with no flaps still reports 0
				// rather than being absent.
				bgpNeighborSessionFlaps.WithLabelValues(s.Key)
			}
		case s.Flaps > p.Flaps:
			bgpNeighborSessionFlaps.WithLabelValues(s.Key).Add(float64(s.Flaps - p.Flaps))
		}

		if s.State == FSMStateEstablished && s.EstablishedTimestamp > 0 {
			bgpNeighborSessionEstablished.WithLabelValues(s.Key).Set(float64(s.EstablishedTimestamp))
		} else {
			// Absent rather than 0 while down — see the metric's declaration.
			bgpNeighborSessionEstablished.DeleteLabelValues(s.Key)
		}
	}

	// Remove peers seen last pass but not this one. Only these are deleted, so
	// there is never a window where a live peer's series is missing.
	for key := range prev {
		if _, still := seen[key]; still {
			continue
		}
		bgpNeighborState.DeletePartialMatch(prometheus.Labels{"neighbor": key})
		bgpNeighborSessionEstablished.DeleteLabelValues(key)
		bgpNeighborSessionFlaps.DeleteLabelValues(key)
	}

	bgpNeighborMetricsTruncated.Set(float64(truncated))
	return seen
}

// DeleteMetricsForConfig removes all metrics for a deleted configuration
func DeleteMetricsForConfig(name, namespace string) {
	bgpNeighborsConfigured.DeleteLabelValues(name, namespace)
	bgpPeerGroupsConfigured.DeleteLabelValues(name, namespace)
	bgpDynamicNeighborsConfigured.DeleteLabelValues(name, namespace)
	bgpConfigurationReady.DeleteLabelValues(name, namespace)
	bgpVrfsConfigured.DeleteLabelValues(name, namespace)
	bgpPoliciesConfigured.DeleteLabelValues(name, namespace)
	bgpDefinedSetsConfigured.DeleteLabelValues(name, namespace)
}

// RecordRouterIDResolution records the result of a router ID resolution attempt
func RecordRouterIDResolution(result string, duration float64) {
	routerIDResolutionTotal.WithLabelValues(result).Inc()
	routerIDResolutionDuration.Observe(duration)
}

// UpdateRouterIDSource updates the active router ID source metric
// This sets the specified source to 1 and resets others to 0
func UpdateRouterIDSource(source string) {
	// Reset all sources - values must match RouterIDSource* constants
	for _, s := range []string{
		RouterIDSourceExplicit,
		RouterIDSourceTemplate,
		RouterIDSourceNodeIPv4,
		RouterIDSourceHashFromNode,
	} {
		if s == source {
			routerIDSource.WithLabelValues(s).Set(1)
		} else {
			routerIDSource.WithLabelValues(s).Set(0)
		}
	}
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

// DeleteRouterIDInfo removes one CR's router ID series. Required on CR
// deletion: without Reset() there is nothing else to clear it, so the series
// would otherwise outlive the CR indefinitely.
func DeleteRouterIDInfo(labels prometheus.Labels) {
	if labels != nil {
		routerIDInfo.Delete(labels)
	}
}
