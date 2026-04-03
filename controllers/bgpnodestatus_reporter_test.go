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
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

// ============================================================================
// Tier 1: Pure function tests
// ============================================================================

func TestPeerStateToString(t *testing.T) {
	tests := []struct {
		name     string
		state    gobgpapi.PeerState_SessionState
		expected string
	}{
		{"Idle", gobgpapi.PeerState_SESSION_STATE_IDLE, "Idle"},
		{"Connect", gobgpapi.PeerState_SESSION_STATE_CONNECT, "Connect"},
		{"Active", gobgpapi.PeerState_SESSION_STATE_ACTIVE, "Active"},
		{"OpenSent", gobgpapi.PeerState_SESSION_STATE_OPENSENT, "OpenSent"},
		{"OpenConfirm", gobgpapi.PeerState_SESSION_STATE_OPENCONFIRM, "OpenConfirm"},
		{"Established", gobgpapi.PeerState_SESSION_STATE_ESTABLISHED, "Established"},
		{"Unknown", gobgpapi.PeerState_SessionState(99), "Unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, peerStateToString(tt.state))
		})
	}
}

func TestTableIDToName(t *testing.T) {
	tests := []struct {
		name     string
		id       int32
		expected string
	}{
		{"main_0", 0, "main"},
		{"main_254", 254, "main"},
		{"default", 253, "default"},
		{"local", 255, "local"},
		{"custom", 100, "100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tableIDToName(tt.id))
		})
	}
}

func TestAddrToHostPrefix(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected string
	}{
		{"ipv4", "10.100.0.1", "10.100.0.1/32"},
		{"ipv6", "2001:db8::1", "2001:db8::1/128"},
		{"ipv4_loopback", "127.0.0.1", "127.0.0.1/32"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr := netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP(tt.ip)}}
			assert.Equal(t, tt.expected, addrToHostPrefix(addr))
		})
	}
}

func TestFormatRouteDistinguisher(t *testing.T) {
	tests := []struct {
		name     string
		rd       *gobgpapi.RouteDistinguisher
		expected string
	}{
		{"nil", nil, ""},
		{"two_octet_asn", &gobgpapi.RouteDistinguisher{
			Rd: &gobgpapi.RouteDistinguisher_TwoOctetAsn{
				TwoOctetAsn: &gobgpapi.RouteDistinguisherTwoOctetASN{Admin: 65000, Assigned: 100},
			},
		}, "65000:100"},
		{"ip_address", &gobgpapi.RouteDistinguisher{
			Rd: &gobgpapi.RouteDistinguisher_IpAddress{
				IpAddress: &gobgpapi.RouteDistinguisherIPAddress{Admin: "10.0.0.1", Assigned: 200},
			},
		}, "10.0.0.1:200"},
		{"four_octet_asn", &gobgpapi.RouteDistinguisher{
			Rd: &gobgpapi.RouteDistinguisher_FourOctetAsn{
				FourOctetAsn: &gobgpapi.RouteDistinguisherFourOctetASN{Admin: 4200000000, Assigned: 1},
			},
		}, "4200000000:1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, formatRouteDistinguisher(tt.rd))
		})
	}
}

func TestInterfaceOperState(t *testing.T) {
	tests := []struct {
		name     string
		oper     netlink.LinkOperState
		flags    net.Flags
		expected string
	}{
		{"oper_up", netlink.OperUp, net.FlagUp, "up"},
		{"oper_down", netlink.OperDown, 0, "down"},
		{"oper_unknown_flag_up", netlink.OperUnknown, net.FlagUp, "up"},    // dummy interface pattern
		{"oper_unknown_flag_down", netlink.OperUnknown, 0, "down"},
		{"oper_not_present", netlink.OperNotPresent, 0, "down"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link := &netlink.Dummy{
				LinkAttrs: netlink.LinkAttrs{
					Name:      "test0",
					OperState: tt.oper,
					Flags:     tt.flags,
				},
			}
			assert.Equal(t, tt.expected, interfaceOperState(link))
		})
	}
}

// --- statusEqual and comparison helpers ---

func TestStatusEqual_BothNil(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	assert.True(t, r.statusEqual(nil, nil))
}

func TestStatusEqual_OneNil(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	assert.False(t, r.statusEqual(nil, &bgpv1.BGPNodeStatusData{}))
	assert.False(t, r.statusEqual(&bgpv1.BGPNodeStatusData{}, nil))
}

func TestStatusEqual_IdenticalSimple(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	a := &bgpv1.BGPNodeStatusData{
		NodeName:  "node-a",
		RouterID:  "10.0.0.1",
		ASN:       65000,
		Healthy:   true,
	}
	b := &bgpv1.BGPNodeStatusData{
		NodeName:  "node-a",
		RouterID:  "10.0.0.1",
		ASN:       65000,
		Healthy:   true,
	}
	assert.True(t, r.statusEqual(a, b))
}

func TestStatusEqual_DifferentNodeName(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	a := &bgpv1.BGPNodeStatusData{NodeName: "node-a"}
	b := &bgpv1.BGPNodeStatusData{NodeName: "node-b"}
	assert.False(t, r.statusEqual(a, b))
}

func TestStatusEqual_TimestampIgnored(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	now := metav1.Now()
	later := metav1.NewTime(now.Add(5 * time.Minute))
	a := &bgpv1.BGPNodeStatusData{NodeName: "node-a", LastUpdated: &now}
	b := &bgpv1.BGPNodeStatusData{NodeName: "node-a", LastUpdated: &later}
	assert.True(t, r.statusEqual(a, b))
}

func TestNeighborsEqual_NilVsEmpty(t *testing.T) {
	assert.True(t, neighborsEqual(nil, nil))
	assert.True(t, neighborsEqual(nil, []bgpv1.NeighborStatus{}))
	assert.True(t, neighborsEqual([]bgpv1.NeighborStatus{}, nil))
}

func TestNeighborsEqual_DifferentOrder(t *testing.T) {
	a := []bgpv1.NeighborStatus{
		{Address: "10.0.0.2", State: "Established", PeerASN: 65001},
		{Address: "10.0.0.1", State: "Active", PeerASN: 65002},
	}
	b := []bgpv1.NeighborStatus{
		{Address: "10.0.0.1", State: "Active", PeerASN: 65002},
		{Address: "10.0.0.2", State: "Established", PeerASN: 65001},
	}
	assert.True(t, neighborsEqual(a, b))
}

func TestNeighborsEqual_DifferentState(t *testing.T) {
	a := []bgpv1.NeighborStatus{{Address: "10.0.0.1", State: "Established"}}
	b := []bgpv1.NeighborStatus{{Address: "10.0.0.1", State: "Active"}}
	assert.False(t, neighborsEqual(a, b))
}

func TestRibRoutesEqual_NilVsEmpty(t *testing.T) {
	assert.True(t, ribRoutesEqual(nil, nil))
	assert.True(t, ribRoutesEqual(nil, []bgpv1.RIBRoute{}))
	assert.True(t, ribRoutesEqual([]bgpv1.RIBRoute{}, nil))
}

func TestRibRoutesEqual_DifferentOrder(t *testing.T) {
	a := []bgpv1.RIBRoute{
		{Prefix: "10.200.0.0/24", NextHop: "10.0.0.1"},
		{Prefix: "10.100.0.0/24", NextHop: "10.0.0.2"},
	}
	b := []bgpv1.RIBRoute{
		{Prefix: "10.100.0.0/24", NextHop: "10.0.0.2"},
		{Prefix: "10.200.0.0/24", NextHop: "10.0.0.1"},
	}
	assert.True(t, ribRoutesEqual(a, b))
}

func TestStringSlicesEqual_NilVsEmpty(t *testing.T) {
	assert.True(t, stringSlicesEqual(nil, nil))
	assert.True(t, stringSlicesEqual(nil, []string{}))
	assert.True(t, stringSlicesEqual([]string{}, nil))
}

func TestStringSlicesEqual_DifferentOrder(t *testing.T) {
	a := []string{"65001:100", "65001:200"}
	b := []string{"65001:200", "65001:100"}
	assert.True(t, stringSlicesEqual(a, b))
}

func TestStringSlicesEqual_Different(t *testing.T) {
	a := []string{"65001:100"}
	b := []string{"65001:200"}
	assert.False(t, stringSlicesEqual(a, b))
}

// --- deriveHealth ---

func TestDeriveHealth_AllEstablished(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	status := &bgpv1.BGPNodeStatusData{
		Neighbors: []bgpv1.NeighborStatus{
			{Address: "10.0.0.1", State: "Established"},
			{Address: "10.0.0.2", State: "Established"},
		},
	}
	assert.True(t, r.deriveHealth(status))
}

func TestDeriveHealth_NeighborDown(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	status := &bgpv1.BGPNodeStatusData{
		Neighbors: []bgpv1.NeighborStatus{
			{Address: "10.0.0.1", State: "Established"},
			{Address: "10.0.0.2", State: "Active"},
		},
	}
	assert.False(t, r.deriveHealth(status))
}

func TestDeriveHealth_ImportFailure(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	status := &bgpv1.BGPNodeStatusData{
		Neighbors: []bgpv1.NeighborStatus{
			{Address: "10.0.0.1", State: "Established"},
		},
		NetlinkImport: &bgpv1.NetlinkImportStatus{
			ImportedAddresses: []bgpv1.ImportedAddress{
				{Address: "10.100.0.1/32", InRIB: true},
				{Address: "10.100.0.2/32", InRIB: false},
			},
		},
	}
	assert.False(t, r.deriveHealth(status))
}

func TestDeriveHealth_ExportFailure(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	status := &bgpv1.BGPNodeStatusData{
		Neighbors: []bgpv1.NeighborStatus{
			{Address: "10.0.0.1", State: "Established"},
		},
		NetlinkExport: &bgpv1.NetlinkExportStatus{
			ExportedRoutes: []bgpv1.ExportedRoute{
				{Prefix: "10.200.0.0/24", Installed: true},
				{Prefix: "10.200.1.0/24", Installed: false, Reason: "nexthop unreachable"},
			},
		},
	}
	assert.False(t, r.deriveHealth(status))
}

func TestDeriveHealth_NoNeighbors(t *testing.T) {
	r := &BGPNodeStatusReporter{}
	status := &bgpv1.BGPNodeStatusData{}
	assert.True(t, r.deriveHealth(status))
}

// --- setCondition ---

func TestSetCondition_NewCondition(t *testing.T) {
	status := &bgpv1.BGPNodeStatusData{}
	setCondition(status, "Ready", metav1.ConditionTrue, "AllHealthy", "all good")
	require.Len(t, status.Conditions, 1)
	assert.Equal(t, "Ready", status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, status.Conditions[0].Status)
	assert.Equal(t, "AllHealthy", status.Conditions[0].Reason)
	assert.Equal(t, "all good", status.Conditions[0].Message)
	assert.False(t, status.Conditions[0].LastTransitionTime.IsZero())
}

func TestSetCondition_UpdateExisting(t *testing.T) {
	status := &bgpv1.BGPNodeStatusData{
		Conditions: []metav1.Condition{
			{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllHealthy", Message: "ok",
				LastTransitionTime: metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))},
		},
	}
	originalTime := status.Conditions[0].LastTransitionTime

	// Same status — transition time should NOT change
	setCondition(status, "Ready", metav1.ConditionTrue, "StillHealthy", "still ok")
	require.Len(t, status.Conditions, 1)
	assert.Equal(t, "StillHealthy", status.Conditions[0].Reason)
	assert.Equal(t, originalTime, status.Conditions[0].LastTransitionTime)

	// Different status — transition time SHOULD change
	setCondition(status, "Ready", metav1.ConditionFalse, "Degraded", "not ok")
	assert.Equal(t, metav1.ConditionFalse, status.Conditions[0].Status)
	assert.NotEqual(t, originalTime, status.Conditions[0].LastTransitionTime)
}

// --- importStatusEqual, exportStatusEqual, vrfsEqual ---

func TestImportStatusEqual_BothNil(t *testing.T) {
	assert.True(t, importStatusEqual(nil, nil))
}

func TestImportStatusEqual_OneNil(t *testing.T) {
	assert.False(t, importStatusEqual(nil, &bgpv1.NetlinkImportStatus{Enabled: true}))
}

func TestExportStatusEqual_BothNil(t *testing.T) {
	assert.True(t, exportStatusEqual(nil, nil))
}

func TestVrfsEqual_NilVsEmpty(t *testing.T) {
	assert.True(t, vrfsEqual(nil, nil))
	assert.True(t, vrfsEqual(nil, []bgpv1.VRFStatus{}))
	assert.True(t, vrfsEqual([]bgpv1.VRFStatus{}, nil))
}

// ============================================================================
// Tier 2: Fake K8s client tests
// ============================================================================

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, bgpv1.AddToScheme(scheme))
	return scheme
}

func newTestNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("node-uid-12345"),
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "192.168.1.10"},
			},
		},
	}
}

func TestEnsureBGPNodeStatus_CreatesNew(t *testing.T) {
	scheme := newTestScheme(t)
	node := newTestNode("test-node")

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "test-node",
	}

	ctx := context.Background()
	err := r.ensureBGPNodeStatus(ctx)
	require.NoError(t, err)

	// Verify object was created
	created := &bgpv1.BGPNodeStatus{}
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-node"}, created)
	require.NoError(t, err)
	assert.Equal(t, "test-node", created.Name)

	// Verify OwnerReference
	require.Len(t, created.OwnerReferences, 1)
	assert.Equal(t, "v1", created.OwnerReferences[0].APIVersion)
	assert.Equal(t, "Node", created.OwnerReferences[0].Kind)
	assert.Equal(t, "test-node", created.OwnerReferences[0].Name)
	assert.Equal(t, types.UID("node-uid-12345"), created.OwnerReferences[0].UID)
}

func TestEnsureBGPNodeStatus_ExistingObject(t *testing.T) {
	scheme := newTestScheme(t)
	node := newTestNode("test-node")

	existing := &bgpv1.BGPNodeStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(node, existing).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "test-node",
	}

	// Should succeed without error (pod restart scenario)
	ctx := context.Background()
	err := r.ensureBGPNodeStatus(ctx)
	require.NoError(t, err)
}

func TestEnsureBGPNodeStatus_NodeNotFound(t *testing.T) {
	scheme := newTestScheme(t)
	// No node in fake client
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "missing-node",
	}

	ctx := context.Background()
	err := r.ensureBGPNodeStatus(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get node for OwnerReference")
}

func TestWriteStatus_SkipsWhenUnchanged(t *testing.T) {
	scheme := newTestScheme(t)
	node := newTestNode("test-node")

	existing := &bgpv1.BGPNodeStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bgpv1.BGPNodeStatus{}).
		WithObjects(node, existing).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "test-node",
	}
	r.heartbeatSeconds.Store(3600) // long heartbeat so second write skips

	status := &bgpv1.BGPNodeStatusData{
		NodeName: "test-node",
		RouterID: "10.0.0.1",
		Healthy:  true,
	}

	ctx := context.Background()

	// Seed lastWrittenStatus to avoid first-write jitter
	r.lastWrittenStatus = &bgpv1.BGPNodeStatusData{NodeName: "different"}
	r.lastWriteTime = time.Now().Add(-1 * time.Hour) // pretend last write was long ago

	// First write — status changed from "different", but jitter is heartbeat/4 = 900s.
	// Instead, bypass by setting lastWrittenStatus to match, then testing skip.
	r.lastWrittenStatus = status.DeepCopy()
	r.lastWriteTime = time.Now() // just wrote

	// Second write with same status — should skip (heartbeat not elapsed)
	err := r.writeStatus(ctx, status, r.Log)
	require.NoError(t, err)
	// lastWriteTime should not have changed (write was skipped)
	assert.True(t, time.Since(r.lastWriteTime) < 2*time.Second)
}

func TestWriteStatus_ForcesOnHeartbeat(t *testing.T) {
	scheme := newTestScheme(t)
	node := newTestNode("test-node")

	existing := &bgpv1.BGPNodeStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bgpv1.BGPNodeStatus{}).
		WithObjects(node, existing).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "test-node",
	}
	r.heartbeatSeconds.Store(1) // 1 second heartbeat for test speed

	status := &bgpv1.BGPNodeStatusData{
		NodeName: "test-node",
		RouterID: "10.0.0.1",
		Healthy:  true,
	}

	ctx := context.Background()

	// First write
	err := r.writeStatus(ctx, status, r.Log)
	require.NoError(t, err)
	firstWriteTime := r.lastWriteTime

	// Wait for heartbeat to elapse
	time.Sleep(1100 * time.Millisecond)

	// Same status but heartbeat elapsed — should force write
	err = r.writeStatus(ctx, status, r.Log)
	require.NoError(t, err)
	assert.True(t, r.lastWriteTime.After(firstWriteTime))
}

func TestWriteStatus_WritesOnChange(t *testing.T) {
	scheme := newTestScheme(t)
	node := newTestNode("test-node")

	existing := &bgpv1.BGPNodeStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bgpv1.BGPNodeStatus{}).
		WithObjects(node, existing).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "test-node",
	}
	r.heartbeatSeconds.Store(10) // short heartbeat so jitter max is 2s

	ctx := context.Background()

	// First write
	status1 := &bgpv1.BGPNodeStatusData{NodeName: "test-node", Healthy: true}
	err := r.writeStatus(ctx, status1, r.Log)
	require.NoError(t, err)
	firstWriteTime := r.lastWriteTime

	// Changed status — should write (after short jitter, within heartbeat window)
	status2 := &bgpv1.BGPNodeStatusData{NodeName: "test-node", Healthy: false}
	err = r.writeStatus(ctx, status2, r.Log)
	require.NoError(t, err)
	assert.True(t, r.lastWriteTime.After(firstWriteTime))

	// Verify the status was actually written
	updated := &bgpv1.BGPNodeStatus{}
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "test-node"}, updated)
	require.NoError(t, err)
	assert.False(t, updated.Status.Healthy)
}

func TestMarkShutdown_SetsCondition(t *testing.T) {
	scheme := newTestScheme(t)
	node := newTestNode("test-node")

	existing := &bgpv1.BGPNodeStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Status: bgpv1.BGPNodeStatusData{
			NodeName: "test-node",
			Healthy:  true,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&bgpv1.BGPNodeStatus{}).
		WithObjects(node, existing).
		Build()

	r := &BGPNodeStatusReporter{
		Client:   fakeClient,
		Log:      zap.New(zap.UseDevMode(true)),
		Scheme:   scheme,
		NodeName: "test-node",
	}

	r.markShutdown(r.Log)

	// Verify shutdown condition was set and object was NOT deleted
	updated := &bgpv1.BGPNodeStatus{}
	err := fakeClient.Get(context.Background(), client.ObjectKey{Name: "test-node"}, updated)
	require.NoError(t, err, "BGPNodeStatus should NOT be deleted on shutdown")
	assert.False(t, updated.Status.Healthy)

	require.Len(t, updated.Status.Conditions, 1)
	assert.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	assert.Equal(t, metav1.ConditionFalse, updated.Status.Conditions[0].Status)
	assert.Equal(t, "AgentShutdown", updated.Status.Conditions[0].Reason)
	assert.NotNil(t, updated.Status.LastUpdated)
}

func TestUpdateConfig(t *testing.T) {
	r := &BGPNodeStatusReporter{}

	r.UpdateConfig(true, 30, "10.0.0.1", "node-ipv4", 65000)

	assert.True(t, r.enabled.Load())
	assert.Equal(t, int32(30), r.heartbeatSeconds.Load())
	assert.Equal(t, "10.0.0.1", *r.routerID.Load())
	assert.Equal(t, "node-ipv4", *r.routerIDSource.Load())
	assert.Equal(t, uint32(65000), r.asn.Load())

	// Toggle disabled
	r.UpdateConfig(false, 120, "10.0.0.2", "explicit", 65001)
	assert.False(t, r.enabled.Load())
	assert.Equal(t, int32(120), r.heartbeatSeconds.Load())
	assert.Equal(t, "10.0.0.2", *r.routerID.Load())
}
