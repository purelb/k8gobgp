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
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	bgpv1 "github.com/purelb/k8gobgp/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// This file exists because every comparison bug this package has had lived in
// the same blind spot: we compare what we sent against what gobgpd returns,
// having never written down what gobgpd returns. The interfaces to do it -
// GoBGPStatsClient, GoBGPNodeStatusClient - were added with the comment
// "allows mocking gRPC calls" and then never used by a test.
//
// gobgpdEcho below is the missing piece. It reproduces the shape of
// oc.NewPeerFromConfigStruct: every sub-message populated whether or not the
// caller set one, some fields resolved, some defaulted, some never echoed at
// all. A comparator that survives it is idempotent against the real daemon.
//
// Keep it faithful. If the fork changes what it returns and this is not
// updated, the tests here go green while production churns - which is exactly
// the failure they were written to prevent.

// gobgpdEcho returns what gobgpd's ListPeer would report for a peer that was
// added with the given config, per pkg/config/oc.NewPeerFromConfigStruct.
func gobgpdEcho(sent *gobgpapi.Peer) *gobgpapi.Peer {
	// Transport.LocalAddress is State.LocalAddress when valid, else
	// Config.LocalAddress - and it is rendered with netip.Addr.String(), which
	// produces "invalid IP" rather than "" for the zero value.
	localAddress := sent.GetTransport().GetLocalAddress()
	if localAddress == "" {
		localAddress = "invalid IP"
	}

	// RouteReflector.RouteReflectorClusterId comes from State, rendered the same
	// way, and gobgpd derives it from the router ID for route-reflector clients.
	clusterID := sent.GetRouteReflector().GetRouteReflectorClusterId()
	if clusterID == "" {
		clusterID = "invalid IP"
	}

	// ListPeer redacts the TCP-MD5 password before the peer leaves the server
	// (pkg/server/server.go, "Redact here, not in the converter"). Added in the
	// fork after v1.1.2, so this is new as of the v1.3.0 bump: a comparator that
	// asserts on AuthPassword is now permanently unequal for every authenticated
	// peer. Copy the Conf so the redaction does not mutate the caller's peer.
	// proto.CloneOf, not a struct copy: a protobuf message embeds a MessageState
	// containing a mutex, so copying it by value trips copylocks.
	conf := proto.CloneOf(sent.GetConf())
	if conf != nil {
		conf.AuthPassword = ""
	}

	echo := &gobgpapi.Peer{
		Conf:     conf,
		AfiSafis: sent.GetAfiSafis(),

		// Always populated, even when the caller sent nothing.
		ApplyPolicy: &gobgpapi.ApplyPolicy{
			// Direction is set; Name never is. crdToAPIPolicyAssignment does the
			// exact opposite.
			ImportPolicy: &gobgpapi.PolicyAssignment{
				Direction:     gobgpapi.PolicyDirection_POLICY_DIRECTION_IMPORT,
				DefaultAction: sent.GetApplyPolicy().GetImportPolicy().GetDefaultAction(),
				Policies:      sent.GetApplyPolicy().GetImportPolicy().GetPolicies(),
			},
			ExportPolicy: &gobgpapi.PolicyAssignment{
				Direction:     gobgpapi.PolicyDirection_POLICY_DIRECTION_EXPORT,
				DefaultAction: sent.GetApplyPolicy().GetExportPolicy().GetDefaultAction(),
				Policies:      sent.GetApplyPolicy().GetExportPolicy().GetPolicies(),
			},
		},
		EbgpMultihop: &gobgpapi.EbgpMultihop{
			Enabled:     sent.GetEbgpMultihop().GetEnabled(),
			MultihopTtl: sent.GetEbgpMultihop().GetMultihopTtl(),
		},
		Timers: &gobgpapi.Timers{
			Config: &gobgpapi.TimersConfig{
				ConnectRetry:      sent.GetTimers().GetConfig().GetConnectRetry(),
				HoldTime:          sent.GetTimers().GetConfig().GetHoldTime(),
				KeepaliveInterval: sent.GetTimers().GetConfig().GetKeepaliveInterval(),
				// Defaulted by gobgpd; the CRD has no field for it. And note what
				// is missing: MinimumAdvertisementInterval is never echoed, no
				// matter what was configured.
				IdleHoldTimeAfterReset: 30,
			},
			State: &gobgpapi.TimersState{KeepaliveInterval: 30},
		},
		RouteReflector: &gobgpapi.RouteReflector{
			RouteReflectorClient:    sent.GetRouteReflector().GetRouteReflectorClient(),
			RouteReflectorClusterId: clusterID,
		},
		GracefulRestart: &gobgpapi.GracefulRestart{
			Enabled:     sent.GetGracefulRestart().GetEnabled(),
			RestartTime: sent.GetGracefulRestart().GetRestartTime(),
			HelperOnly:  sent.GetGracefulRestart().GetHelperOnly(),
			// State-sourced and config-defaulted fields the CRD cannot express.
			DeferralTime:    360,
			LocalRestarting: false,
		},
		Transport: &gobgpapi.Transport{
			LocalAddress:  localAddress,
			PassiveMode:   sent.GetTransport().GetPassiveMode(),
			BindInterface: sent.GetTransport().GetBindInterface(),
			// Runtime values the caller never sets.
			RemotePort: 179,
			LocalPort:  45678,
		},
		State: &gobgpapi.PeerState{
			NeighborAddress: sent.GetConf().GetNeighborAddress(),
			SessionState:    gobgpapi.PeerState_SESSION_STATE_ESTABLISHED,
			Messages:        &gobgpapi.Messages{},
		},
	}
	return echo
}

// --- fake gRPC client -------------------------------------------------------

type fakeListPeerStream struct {
	grpc.ClientStream
	peers []*gobgpapi.Peer
	next  int
}

func (s *fakeListPeerStream) Recv() (*gobgpapi.ListPeerResponse, error) {
	if s.next >= len(s.peers) {
		return nil, io.EOF
	}
	p := s.peers[s.next]
	s.next++
	return &gobgpapi.ListPeerResponse{Peer: p}, nil
}

// fakeGoBGP implements only the RPCs reconcileNeighbors uses. The embedded
// interface is nil, so any other call panics - which is the point: a test that
// starts exercising a new RPC should have to say so.
type fakeGoBGP struct {
	gobgpapi.GoBgpServiceClient
	peers []*gobgpapi.Peer

	addPeer    int
	updatePeer int
	deletePeer int
}

func (f *fakeGoBGP) ListPeer(_ context.Context, _ *gobgpapi.ListPeerRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[gobgpapi.ListPeerResponse], error) {
	return &fakeListPeerStream{peers: f.peers}, nil
}

func (f *fakeGoBGP) AddPeer(_ context.Context, _ *gobgpapi.AddPeerRequest, _ ...grpc.CallOption) (*gobgpapi.AddPeerResponse, error) {
	f.addPeer++
	return &gobgpapi.AddPeerResponse{}, nil
}

func (f *fakeGoBGP) UpdatePeer(_ context.Context, _ *gobgpapi.UpdatePeerRequest, _ ...grpc.CallOption) (*gobgpapi.UpdatePeerResponse, error) {
	f.updatePeer++
	return &gobgpapi.UpdatePeerResponse{}, nil
}

func (f *fakeGoBGP) DeletePeer(_ context.Context, _ *gobgpapi.DeletePeerRequest, _ ...grpc.CallOption) (*gobgpapi.DeletePeerResponse, error) {
	f.deletePeer++
	return &gobgpapi.DeletePeerResponse{}, nil
}

// --- the test that matters --------------------------------------------------

// TestReconcileNeighbors_Idempotent is the regression guard for the reconcile
// churn. It reconciles against a daemon that already holds the desired config
// and asserts that nothing is written.
//
// Before the comparator fixes this failed on every case: gobgpd reports six
// sub-messages the CRD leaves nil, so peerConfigEqual returned false forever and
// UpdatePeer fired once per neighbor per reconcile. Harmless in isolation -
// NeedsResendOpenMessage kept sessions up - but an UpdatePeer on a BFD-enabled
// peer tears the BFD session down, so this becomes a fleet-wide flap the moment
// BFD is configurable.
func TestReconcileNeighbors_Idempotent(t *testing.T) {
	tests := []struct {
		name     string
		neighbor bgpv1.Neighbor
	}{
		{
			name: "minimal neighbor, every optional block omitted",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1", PeerAsn: 64513},
			},
		},
		{
			name: "transport set but localAddress left empty",
			neighbor: bgpv1.Neighbor{
				Config:    bgpv1.NeighborConfig{NeighborAddress: "10.0.0.2", PeerAsn: 64513},
				Transport: &bgpv1.Transport{PassiveMode: true},
			},
		},
		{
			name: "timers set, including a field gobgpd never echoes",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.3", PeerAsn: 64513},
				Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{
					HoldTime:                     90,
					KeepaliveInterval:            30,
					MinimumAdvertisementInterval: 5,
				}},
			},
		},
		{
			name: "apply policy set - we send Name, gobgpd returns Direction",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.4", PeerAsn: 64513},
				ApplyPolicy: &bgpv1.ApplyPolicy{
					ImportPolicy: &bgpv1.PolicyAssignment{
						Name:          "import",
						Policies:      []string{"p1"},
						DefaultAction: "reject",
					},
				},
			},
		},
		{
			name: "graceful restart set - gobgpd echoes six more fields",
			neighbor: bgpv1.Neighbor{
				Config:          bgpv1.NeighborConfig{NeighborAddress: "10.0.0.5", PeerAsn: 64513},
				GracefulRestart: &bgpv1.GracefulRestart{Enabled: true, RestartTime: 120},
			},
		},
		{
			name: "route reflector client without an explicit cluster ID",
			neighbor: bgpv1.Neighbor{
				Config:         bgpv1.NeighborConfig{NeighborAddress: "10.0.0.6", PeerAsn: 64513},
				RouteReflector: &bgpv1.RouteReflector{RouteReflectorClient: true},
			},
		},
		{
			name: "ebgp multihop set",
			neighbor: bgpv1.Neighbor{
				Config:       bgpv1.NeighborConfig{NeighborAddress: "10.0.0.7", PeerAsn: 64513},
				EbgpMultihop: &bgpv1.EbgpMultihop{Enabled: true, MultihopTtl: 3},
			},
		},
		{
			name: "unnumbered neighbor",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborInterface: "eth0", PeerAsn: 64513},
			},
		},
		{
			// ListPeer redacts the password, so the comparator can never see it
			// come back. Asserting on it means an UpdatePeer every reconcile for
			// every authenticated peer.
			name: "MD5-authenticated neighbor",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{
					NeighborAddress: "10.0.0.8",
					PeerAsn:         64513,
					AuthPassword:    "correct horse battery staple",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &bgpv1.BGPConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: bgpv1.BGPConfigurationSpec{
					Global:    bgpv1.GlobalSpec{ASN: 64512},
					Neighbors: []bgpv1.Neighbor{tc.neighbor},
				},
			}

			r := &BGPConfigurationReconciler{Log: logf.Log}

			// What the reconciler would send for this CR, then what gobgpd
			// would report back afterwards.
			sent := r.crdToAPINeighborWithPassword(&tc.neighbor, "")
			require.NotNil(t, sent, "converter returned nil")

			fake := &fakeGoBGP{peers: []*gobgpapi.Peer{gobgpdEcho(sent)}}

			err := r.reconcileNeighbors(context.Background(), fake, cfg, map[string]*gobgpapi.PeerGroup{}, logf.Log)
			require.NoError(t, err)

			assert.Zero(t, fake.updatePeer,
				"UpdatePeer called for an unchanged neighbor: the comparator disagrees with what gobgpd reports")
			assert.Zero(t, fake.addPeer, "AddPeer called for a neighbor gobgpd already has")
			assert.Zero(t, fake.deletePeer, "DeletePeer called for a neighbor that should be kept")
		})
	}
}

// TestReconcileNeighbors_DetectsRealChange is the other half: the comparator
// must still notice an actual edit. A comparator that returns true
// unconditionally would pass the idempotency test above.
func TestReconcileNeighbors_DetectsRealChange(t *testing.T) {
	existing := bgpv1.Neighbor{
		Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1", PeerAsn: 64513},
		Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{HoldTime: 90, KeepaliveInterval: 30}},
	}
	changed := bgpv1.Neighbor{
		Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.1", PeerAsn: 64513},
		Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{HoldTime: 180, KeepaliveInterval: 60}},
	}

	cfg := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: bgpv1.BGPConfigurationSpec{
			Global:    bgpv1.GlobalSpec{ASN: 64512},
			Neighbors: []bgpv1.Neighbor{changed},
		},
	}

	r := &BGPConfigurationReconciler{Log: logf.Log}
	fake := &fakeGoBGP{peers: []*gobgpapi.Peer{gobgpdEcho(r.crdToAPINeighborWithPassword(&existing, ""))}}

	require.NoError(t, r.reconcileNeighbors(context.Background(), fake, cfg, map[string]*gobgpapi.PeerGroup{}, logf.Log))
	assert.Equal(t, 1, fake.updatePeer, "a changed hold time must produce exactly one UpdatePeer")
}
