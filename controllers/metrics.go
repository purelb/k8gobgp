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

	// BGP session metrics
	bgpNeighborsConfigured = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbors_configured",
			Help: "Number of BGP neighbors configured",
		},
		[]string{"name", "namespace"},
	)

	bgpNeighborsEstablished = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "k8gobgp_neighbors_established",
			Help: "Number of BGP neighbors in established state",
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
	bgpConfigurationCount = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "k8gobgp_configurations_total",
			Help: "Total number of BGPConfiguration resources",
		},
	)

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
)

func init() {
	// Register all metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		reconcileTotal,
		reconcileDuration,
		bgpNeighborsConfigured,
		bgpNeighborsEstablished,
		bgpPeerGroupsConfigured,
		bgpDynamicNeighborsConfigured,
		gobgpConnectionStatus,
		gobgpConnectionErrors,
		bgpConfigurationCount,
		bgpConfigurationReady,
		bgpVrfsConfigured,
		bgpPoliciesConfigured,
		bgpDefinedSetsConfigured,
		cleanupRetries,
		cleanupDuration,
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

// UpdateNeighborMetrics updates the neighbor count metrics
func UpdateNeighborMetrics(name, namespace string, configured, established int) {
	bgpNeighborsConfigured.WithLabelValues(name, namespace).Set(float64(configured))
	bgpNeighborsEstablished.WithLabelValues(name, namespace).Set(float64(established))
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

// UpdateConfigurationCount updates the total configuration count
func UpdateConfigurationCount(count int) {
	bgpConfigurationCount.Set(float64(count))
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

// DeleteMetricsForConfig removes all metrics for a deleted configuration
func DeleteMetricsForConfig(name, namespace string) {
	bgpNeighborsConfigured.DeleteLabelValues(name, namespace)
	bgpNeighborsEstablished.DeleteLabelValues(name, namespace)
	bgpPeerGroupsConfigured.DeleteLabelValues(name, namespace)
	bgpDynamicNeighborsConfigured.DeleteLabelValues(name, namespace)
	bgpConfigurationReady.DeleteLabelValues(name, namespace)
	bgpVrfsConfigured.DeleteLabelValues(name, namespace)
	bgpPoliciesConfigured.DeleteLabelValues(name, namespace)
	bgpDefinedSetsConfigured.DeleteLabelValues(name, namespace)
}
