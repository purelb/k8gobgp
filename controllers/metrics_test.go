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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestUpdateConfiguredObjects(t *testing.T) {
	bgpConfiguredObjects.Reset()

	UpdateConfiguredObjects(KindNeighbor, "c", "ns", 5)
	UpdateConfiguredObjects(KindPeerGroup, "c", "ns", 3)
	UpdateConfiguredObjects(KindVrf, "c", "ns", 2)

	assert.Equal(t, float64(5), testutil.ToFloat64(bgpConfiguredObjects.WithLabelValues(KindNeighbor, "c", "ns")))
	assert.Equal(t, float64(3), testutil.ToFloat64(bgpConfiguredObjects.WithLabelValues(KindPeerGroup, "c", "ns")))
	assert.Equal(t, float64(2), testutil.ToFloat64(bgpConfiguredObjects.WithLabelValues(KindVrf, "c", "ns")))

	// Kinds are independent: one CR's neighbor count must not disturb another's.
	UpdateConfiguredObjects(KindNeighbor, "other", "ns", 1)
	assert.Equal(t, float64(5), testutil.ToFloat64(bgpConfiguredObjects.WithLabelValues(KindNeighbor, "c", "ns")))
}

func TestRecordPeerApplyError(t *testing.T) {
	peerApplyErrors.Reset()

	RecordPeerApplyError("10.0.0.1", "add")
	RecordPeerApplyError("10.0.0.1", "add")
	RecordPeerApplyError("10.0.0.2", "update")

	assert.Equal(t, float64(2), testutil.ToFloat64(peerApplyErrors.WithLabelValues("10.0.0.1", "add")))
	assert.Equal(t, float64(1), testutil.ToFloat64(peerApplyErrors.WithLabelValues("10.0.0.2", "update")))
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
	bgpConfiguredObjects.Reset()
	bgpConfigurationReady.Reset()

	UpdateConfiguredObjects(KindNeighbor, "delete-test", "test-ns", 5)
	UpdateConfiguredObjects(KindVrf, "delete-test", "test-ns", 2)
	UpdateConfigurationReadyStatus("delete-test", "test-ns", true)

	// A second CR whose series must survive the first one's deletion.
	UpdateConfiguredObjects(KindNeighbor, "keep", "test-ns", 9)

	require.Equal(t, 3, testutil.CollectAndCount(bgpConfiguredObjects))

	DeleteMetricsForConfig("delete-test", "test-ns")

	// Every kind for that CR is gone - not just the one the old test checked -
	// and the other CR is untouched.
	assert.Equal(t, 1, testutil.CollectAndCount(bgpConfiguredObjects))
	assert.Equal(t, float64(9), testutil.ToFloat64(bgpConfiguredObjects.WithLabelValues(KindNeighbor, "keep", "test-ns")))
	assert.Equal(t, 0, testutil.CollectAndCount(bgpConfigurationReady))
}

func TestRecordRouterIDResolution(t *testing.T) {
	routerIDResolutionTotal.Reset()

	RecordRouterIDResolution("success", 0.05)
	RecordRouterIDResolution("success", 0.10)
	RecordRouterIDResolution("failure", 0.02)

	successCount := testutil.ToFloat64(routerIDResolutionTotal.WithLabelValues("success"))
	assert.Equal(t, float64(2), successCount)

	failureCount := testutil.ToFloat64(routerIDResolutionTotal.WithLabelValues("failure"))
	assert.Equal(t, float64(1), failureCount)
}

func TestUpdateRouterIDInfo(t *testing.T) {
	routerIDInfo.Reset()

	first := prometheus.Labels{
		"router_id": "10.0.0.5", "source": "node-ipv4", "node": "worker-1",
		"asn": "64512", "name": "cfg", "namespace": "default",
	}
	UpdateRouterIDInfo(nil, first)
	assert.Equal(t, float64(1), testutil.ToFloat64(routerIDInfo.With(first)))

	second := prometheus.Labels{
		"router_id": "10.255.0.42", "source": "hash-from-node-name", "node": "worker-2",
		"asn": "64513", "name": "cfg", "namespace": "default",
	}
	UpdateRouterIDInfo(first, second)
	assert.Equal(t, float64(1), testutil.ToFloat64(routerIDInfo.With(second)))
	assert.Equal(t, float64(0), testutil.ToFloat64(routerIDInfo.With(first)))
}

// --- Metrics overhaul ---------------------------------------------------------

// Two BGPConfigurations on one node must not erase each other's series — the
// failure the previous Reset()-based implementation produced.
func TestUpdateRouterIDInfo_MultipleCRsCoexist(t *testing.T) {
	routerIDInfo.Reset()

	crA := prometheus.Labels{"router_id": "10.0.0.1", "source": "node-ipv4", "node": "n1", "asn": "64512", "name": "a", "namespace": "default"}
	crB := prometheus.Labels{"router_id": "10.0.0.2", "source": "node-ipv4", "node": "n1", "asn": "64512", "name": "b", "namespace": "default"}

	UpdateRouterIDInfo(nil, crA)
	UpdateRouterIDInfo(nil, crB)
	assert.Equal(t, float64(1), testutil.ToFloat64(routerIDInfo.With(crA)))
	assert.Equal(t, float64(1), testutil.ToFloat64(routerIDInfo.With(crB)))

	DeleteRouterIDInfo(crA)
	assert.Equal(t, float64(0), testutil.ToFloat64(routerIDInfo.With(crA)))
	assert.Equal(t, float64(1), testutil.ToFloat64(routerIDInfo.With(crB)))
	assert.NotPanics(t, func() { DeleteRouterIDInfo(nil) })
}
