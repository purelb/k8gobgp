package controllers

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestRecordReconcileResult(t *testing.T) {
	// Reset counter for testing
	reconcileTotal.Reset()

	RecordReconcileResult("test-config", "test-ns", "success")
	RecordReconcileResult("test-config", "test-ns", "success")
	RecordReconcileResult("test-config", "test-ns", "failed")

	// Check success count
	successCount := testutil.ToFloat64(reconcileTotal.WithLabelValues("test-config", "test-ns", "success"))
	assert.Equal(t, float64(2), successCount)

	// Check failed count
	failedCount := testutil.ToFloat64(reconcileTotal.WithLabelValues("test-config", "test-ns", "failed"))
	assert.Equal(t, float64(1), failedCount)
}

func TestRecordReconcileDuration(t *testing.T) {
	// Reset histogram for testing
	reconcileDuration.Reset()

	// Record some durations
	RecordReconcileDuration("test-config", "test-ns", 0.5)
	RecordReconcileDuration("test-config", "test-ns", 1.0)

	// Just verify no panic - histogram internals are harder to test
	// The function should execute without error
}

func TestUpdateNeighborMetrics(t *testing.T) {
	// Reset gauges
	bgpNeighborsConfigured.Reset()
	bgpNeighborsEstablished.Reset()

	UpdateNeighborMetrics("test-config", "test-ns", 5, 3)

	configured := testutil.ToFloat64(bgpNeighborsConfigured.WithLabelValues("test-config", "test-ns"))
	established := testutil.ToFloat64(bgpNeighborsEstablished.WithLabelValues("test-config", "test-ns"))

	assert.Equal(t, float64(5), configured)
	assert.Equal(t, float64(3), established)
}

func TestUpdatePeerGroupMetrics(t *testing.T) {
	bgpPeerGroupsConfigured.Reset()

	UpdatePeerGroupMetrics("test-config", "test-ns", 3)

	count := testutil.ToFloat64(bgpPeerGroupsConfigured.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(3), count)
}

func TestUpdateDynamicNeighborMetrics(t *testing.T) {
	bgpDynamicNeighborsConfigured.Reset()

	UpdateDynamicNeighborMetrics("test-config", "test-ns", 2)

	count := testutil.ToFloat64(bgpDynamicNeighborsConfigured.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(2), count)
}

func TestRecordGoBGPConnection(t *testing.T) {
	// Reset gauge
	gobgpConnectionStatus.Reset()

	RecordGoBGPConnection("localhost:50051", true)
	status := testutil.ToFloat64(gobgpConnectionStatus.WithLabelValues("localhost:50051"))
	assert.Equal(t, float64(1), status)

	RecordGoBGPConnection("localhost:50051", false)
	status = testutil.ToFloat64(gobgpConnectionStatus.WithLabelValues("localhost:50051"))
	assert.Equal(t, float64(0), status)
}

func TestRecordGoBGPConnectionError(t *testing.T) {
	// Reset counter
	gobgpConnectionErrors.Reset()

	RecordGoBGPConnectionError("localhost:50051")
	RecordGoBGPConnectionError("localhost:50051")

	count := testutil.ToFloat64(gobgpConnectionErrors.WithLabelValues("localhost:50051"))
	assert.Equal(t, float64(2), count)
}

func TestUpdateConfigurationCount(t *testing.T) {
	UpdateConfigurationCount(5)
	count := testutil.ToFloat64(bgpConfigurationCount)
	assert.Equal(t, float64(5), count)

	UpdateConfigurationCount(10)
	count = testutil.ToFloat64(bgpConfigurationCount)
	assert.Equal(t, float64(10), count)
}

func TestUpdateConfigurationReadyStatus(t *testing.T) {
	// Reset gauge
	bgpConfigurationReady.Reset()

	UpdateConfigurationReadyStatus("test-config", "test-ns", true)
	ready := testutil.ToFloat64(bgpConfigurationReady.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(1), ready)

	UpdateConfigurationReadyStatus("test-config", "test-ns", false)
	ready = testutil.ToFloat64(bgpConfigurationReady.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(0), ready)
}

func TestUpdateVrfMetrics(t *testing.T) {
	bgpVrfsConfigured.Reset()

	UpdateVrfMetrics("test-config", "test-ns", 4)

	count := testutil.ToFloat64(bgpVrfsConfigured.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(4), count)
}

func TestUpdatePolicyMetrics(t *testing.T) {
	// Reset gauges
	bgpPoliciesConfigured.Reset()
	bgpDefinedSetsConfigured.Reset()

	UpdatePolicyMetrics("test-config", "test-ns", 3, 5)

	policies := testutil.ToFloat64(bgpPoliciesConfigured.WithLabelValues("test-config", "test-ns"))
	definedSets := testutil.ToFloat64(bgpDefinedSetsConfigured.WithLabelValues("test-config", "test-ns"))

	assert.Equal(t, float64(3), policies)
	assert.Equal(t, float64(5), definedSets)
}

func TestRecordCleanupRetry(t *testing.T) {
	// Reset counter
	cleanupRetries.Reset()

	RecordCleanupRetry("test-config", "test-ns")
	RecordCleanupRetry("test-config", "test-ns")
	RecordCleanupRetry("test-config", "test-ns")

	count := testutil.ToFloat64(cleanupRetries.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(3), count)
}

func TestRecordCleanupDuration(t *testing.T) {
	// Reset histogram
	cleanupDuration.Reset()

	// Record some durations - just verify no panic
	RecordCleanupDuration("test-config", "test-ns", 0.1)
	RecordCleanupDuration("test-config", "test-ns", 0.5)
}

func TestDeleteMetricsForConfig(t *testing.T) {
	// Set up some metrics
	bgpNeighborsConfigured.Reset()
	bgpConfigurationReady.Reset()

	UpdateNeighborMetrics("delete-test", "test-ns", 5, 3)
	UpdateConfigurationReadyStatus("delete-test", "test-ns", true)

	// Verify metrics exist
	configured := testutil.ToFloat64(bgpNeighborsConfigured.WithLabelValues("delete-test", "test-ns"))
	assert.Equal(t, float64(5), configured)

	// Delete metrics - this removes the label values from the metric
	DeleteMetricsForConfig("delete-test", "test-ns")

	// After deletion, the metric label values are removed
	// Accessing them again creates new ones initialized to 0
	configured = testutil.ToFloat64(bgpNeighborsConfigured.WithLabelValues("delete-test", "test-ns"))
	assert.Equal(t, float64(0), configured)
}
