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
	"strings"
	"testing"

	gobgpapi "github.com/osrg/gobgp/v4/api"

	"github.com/prometheus/client_golang/prometheus"
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

func TestUpdateNeighborsConfigured(t *testing.T) {
	bgpNeighborsConfigured.Reset()

	UpdateNeighborsConfigured("test-config", "test-ns", 5)

	configured := testutil.ToFloat64(bgpNeighborsConfigured.WithLabelValues("test-config", "test-ns"))
	assert.Equal(t, float64(5), configured)
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

	UpdateNeighborsConfigured("delete-test", "test-ns", 5)
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

func TestUpdateRouterIDSource(t *testing.T) {
	routerIDSource.Reset()

	UpdateRouterIDSource(RouterIDSourceNodeIPv4)

	// Active source should be 1
	activeVal := testutil.ToFloat64(routerIDSource.WithLabelValues(RouterIDSourceNodeIPv4))
	assert.Equal(t, float64(1), activeVal)

	// All other sources should be 0
	explicitVal := testutil.ToFloat64(routerIDSource.WithLabelValues(RouterIDSourceExplicit))
	assert.Equal(t, float64(0), explicitVal)

	templateVal := testutil.ToFloat64(routerIDSource.WithLabelValues(RouterIDSourceTemplate))
	assert.Equal(t, float64(0), templateVal)

	hashVal := testutil.ToFloat64(routerIDSource.WithLabelValues(RouterIDSourceHashFromNode))
	assert.Equal(t, float64(0), hashVal)

	// Switch source — previous active should become 0
	UpdateRouterIDSource(RouterIDSourceExplicit)

	explicitVal = testutil.ToFloat64(routerIDSource.WithLabelValues(RouterIDSourceExplicit))
	assert.Equal(t, float64(1), explicitVal)

	nodeVal := testutil.ToFloat64(routerIDSource.WithLabelValues(RouterIDSourceNodeIPv4))
	assert.Equal(t, float64(0), nodeVal)
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

// The metric label values are declared independently of peerStateToString so a
// CRD casing change cannot silently rewrite every metric label. This keeps the
// two sets in step by failing loudly if they ever diverge.
func TestFSMStateLabelsMatchCRDStates(t *testing.T) {
	all := []gobgpapi.PeerState_SessionState{
		gobgpapi.PeerState_SESSION_STATE_IDLE,
		gobgpapi.PeerState_SESSION_STATE_CONNECT,
		gobgpapi.PeerState_SESSION_STATE_ACTIVE,
		gobgpapi.PeerState_SESSION_STATE_OPENSENT,
		gobgpapi.PeerState_SESSION_STATE_OPENCONFIRM,
		gobgpapi.PeerState_SESSION_STATE_ESTABLISHED,
		gobgpapi.PeerState_SESSION_STATE_UNSPECIFIED,
	}
	for _, s := range all {
		assert.True(t, strings.EqualFold(fsmStateLabel(s), peerStateToString(s)),
			"metric label %q and CRD state %q have diverged for %v",
			fsmStateLabel(s), peerStateToString(s), s)
	}

	// Every produced label must be a declared one.
	declared := map[string]bool{}
	for _, s := range AllFSMStates {
		declared[s] = true
	}
	for _, s := range all {
		assert.True(t, declared[fsmStateLabel(s)], "undeclared label %q", fsmStateLabel(s))
	}
	assert.Len(t, AllFSMStates, 7)
}

// The previous switch counted only established/active/idle, so a peer
// mid-handshake was counted nowhere and the states did not sum to the total.
func TestSetNeighborStateCounts_AllStatesPresentAndSumToTotal(t *testing.T) {
	bgpNeighbors.Reset()

	SetNeighborStateCounts(map[string]int{
		FSMStateEstablished: 3,
		FSMStateOpenSent:    1,
		FSMStateConnect:     2,
	})

	sum := 0.0
	for _, state := range AllFSMStates {
		// Every state must exist as a series, including the zero ones —
		// "absent" and "zero" mean different things to an alert.
		sum += testutil.ToFloat64(bgpNeighbors.WithLabelValues(state))
	}
	assert.Equal(t, float64(6), sum, "per-state counts must sum to the total")
	assert.Equal(t, float64(1), testutil.ToFloat64(bgpNeighbors.WithLabelValues(FSMStateOpenSent)))
	assert.Equal(t, float64(0), testutil.ToFloat64(bgpNeighbors.WithLabelValues(FSMStateIdle)))
}

func TestSetNeighborMetrics_InfoMetricShapeAndTransitions(t *testing.T) {
	bgpNeighborState.Reset()
	bgpNeighborSessionEstablished.Reset()
	bgpNeighborSessionFlaps.Reset()

	prev := SetNeighborMetrics([]NeighborSample{
		{Key: "10.0.0.1", State: FSMStateEstablished, Flaps: 0, EstablishedTimestamp: 1000},
		{Key: "iface:eth0", State: FSMStateIdle},
	}, 0, nil)

	// One series per peer, carrying the state as a label.
	assert.Equal(t, float64(1), testutil.ToFloat64(bgpNeighborState.WithLabelValues("10.0.0.1", FSMStateEstablished)))
	assert.Equal(t, float64(1000), testutil.ToFloat64(bgpNeighborSessionEstablished.WithLabelValues("10.0.0.1")))
	// Absent, not zero, while down — a zero would read as a 1970 timestamp and
	// poison any "session age" query. Only the established peer has a series.
	assert.Equal(t, 1,
		testutil.CollectAndCount(bgpNeighborSessionEstablished, "k8gobgp_neighbor_session_established_timestamp_seconds"),
		"only the established peer should have a timestamp series")

	// A state transition must not leave the old state series behind.
	prev = SetNeighborMetrics([]NeighborSample{
		{Key: "10.0.0.1", State: FSMStateIdle},
		{Key: "iface:eth0", State: FSMStateEstablished, EstablishedTimestamp: 2000},
	}, 0, prev)

	assert.Equal(t, float64(0), testutil.ToFloat64(bgpNeighborState.WithLabelValues("10.0.0.1", FSMStateEstablished)),
		"stale state series must be removed on transition")
	assert.Equal(t, float64(1), testutil.ToFloat64(bgpNeighborState.WithLabelValues("10.0.0.1", FSMStateIdle)))
	// The peer that went down must lose its timestamp series entirely.
	assert.Equal(t, float64(0), testutil.ToFloat64(bgpNeighborSessionEstablished.WithLabelValues("10.0.0.1")))

	// A departed peer's series are deleted; survivors are untouched.
	SetNeighborMetrics([]NeighborSample{
		{Key: "iface:eth0", State: FSMStateEstablished, EstablishedTimestamp: 2000},
	}, 0, prev)
	assert.Equal(t, float64(0), testutil.ToFloat64(bgpNeighborState.WithLabelValues("10.0.0.1", FSMStateIdle)),
		"departed peer's series must be removed")
	assert.Equal(t, float64(1), testutil.ToFloat64(bgpNeighborState.WithLabelValues("iface:eth0", FSMStateEstablished)))
}

// gobgpd reports an absolute flap count; a Prometheus counter only accepts
// increments and must never go backwards.
func TestSetNeighborMetrics_FlapCounterSemantics(t *testing.T) {
	bgpNeighborSessionFlaps.Reset()
	bgpNeighborState.Reset()

	s := func(flaps uint32) []NeighborSample {
		return []NeighborSample{{Key: "10.0.0.1", State: FSMStateEstablished, Flaps: flaps, EstablishedTimestamp: 1}}
	}

	prev := SetNeighborMetrics(s(2), 0, nil) // first sighting adopts the absolute value
	assert.Equal(t, float64(2), testutil.ToFloat64(bgpNeighborSessionFlaps.WithLabelValues("10.0.0.1")))

	prev = SetNeighborMetrics(s(5), 0, prev) // +3
	assert.Equal(t, float64(5), testutil.ToFloat64(bgpNeighborSessionFlaps.WithLabelValues("10.0.0.1")))

	prev = SetNeighborMetrics(s(5), 0, prev) // unchanged
	assert.Equal(t, float64(5), testutil.ToFloat64(bgpNeighborSessionFlaps.WithLabelValues("10.0.0.1")))

	// gobgpd restarted and began counting again: adopt the new absolute value
	// rather than subtracting into a negative.
	SetNeighborMetrics(s(1), 0, prev)
	assert.Equal(t, float64(6), testutil.ToFloat64(bgpNeighborSessionFlaps.WithLabelValues("10.0.0.1")))
}

func TestSetNeighborMetrics_TruncationIsVisible(t *testing.T) {
	bgpNeighborMetricsTruncated.Set(0)
	SetNeighborMetrics([]NeighborSample{{Key: "10.0.0.1", State: FSMStateIdle}}, 42, nil)
	assert.Equal(t, float64(42), testutil.ToFloat64(bgpNeighborMetricsTruncated),
		"peers beyond the cap must be countable, not silently missing")
}

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
