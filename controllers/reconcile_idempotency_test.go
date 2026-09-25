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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	bgpv1 "github.com/purelb/k8gobgp/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
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
// echoGlobalASN is the global ASN the fake gobgpd defaults an unset per-peer
// local AS to. Arbitrary, but it must differ from anything a fixture sets as a
// peer AS, or the defaulting would be invisible.
const echoGlobalASN = 64512

// defaultTimer mirrors gobgpd applying its default to an unset timer.
func defaultTimer(sent, def uint64) uint64 {
	if sent == 0 {
		return def
	}
	return sent
}

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
		conf.AuthPassword = proto.String("")
		// gobgpd round-trips SendCommunity as of v1.3.1: SendCommunityToAPI
		// returns nil for an unset CommunityType rather than a fabricated 0, so
		// absence survives rather than coming back as "standard". Cloning Conf
		// already reproduces that; this comment records why nothing special is
		// needed here, unlike the fields above.
		// gobgpd defaults an unset per-peer local AS to the global ASN and
		// echoes the defaulted value. Measured on a live v1.3.0: a CR setting
		// only peerAsn comes back with local_asn set to global.asn. Almost no
		// neighbor overrides its local AS, so this is the common case, and
		// echoing it as sent is what let the churn reach production.
		if conf.GetLocalAsn() == 0 {
			conf.LocalAsn = proto.Uint32(echoGlobalASN)
		}
		// The read path reports every explicit-presence field unconditionally -
		// NewPeerFromConfigStruct wraps each one in proto.X and its comment says
		// "Always reported, never nil on the read path". A fake that echoed our
		// nils back would let a comparator that ignores presence look correct,
		// which is precisely how the original UpdatePeer churn reached
		// production. With no peer group in play the resolved value is the zero
		// value, so nil becomes a pointer to zero.
		if conf.Description == nil {
			conf.Description = proto.String("")
		}
		if conf.AllowOwnAsn == nil {
			conf.AllowOwnAsn = proto.Uint32(0)
		}
		if conf.ReplacePeerAsn == nil {
			conf.ReplacePeerAsn = proto.Bool(false)
		}
		if conf.AllowAspathLoopLocal == nil {
			conf.AllowAspathLoopLocal = proto.Bool(false)
		}
		if conf.SendSoftwareVersion == nil {
			conf.SendSoftwareVersion = proto.Bool(false)
		}
		if conf.RemovePrivate == nil {
			rp := gobgpapi.RemovePrivate_REMOVE_PRIVATE_UNSPECIFIED
			conf.RemovePrivate = &rp
		}
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
		// Timers are echoed DEFAULTED, not as sent. This fake originally echoed
		// them as sent, which is why the idempotency tests passed while
		// production fired UpdatePeer on every reconcile for a peer that set
		// holdTime and keepaliveInterval but not connectRetry: 0 against 120,
		// permanently unequal.
		//
		// The values are measured, not remembered - `gobgp neighbor add` with no
		// timers against a live gobgpd v1.3.0, read back with `gobgp -j
		// neighbor`. Note MinimumAdvertisementInterval is never echoed at all, no
		// matter what was configured.
		Timers: &gobgpapi.Timers{
			Config: &gobgpapi.TimersConfig{
				ConnectRetry:           defaultTimer(sent.GetTimers().GetConfig().GetConnectRetry(), 120),
				HoldTime:               defaultTimer(sent.GetTimers().GetConfig().GetHoldTime(), 90),
				KeepaliveInterval:      defaultTimer(sent.GetTimers().GetConfig().GetKeepaliveInterval(), 30),
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
		// Emitted for every peer, and defaulted whether or not BFD is enabled -
		// SetDefaultNeighborConfigValues has no Enabled gate. A peer with no BFD
		// at all still gets 3784/3/1e6/1e6 back, which is the case that churns.
		Bfd: &gobgpapi.BfdPeerConfig{
			Enabled:                  sent.GetBfd().GetEnabled(),
			Port:                     orDefault(sent.GetBfd().GetPort(), 3784),
			DesiredMinimumTxInterval: orDefault(sent.GetBfd().GetDesiredMinimumTxInterval(), 1000000),
			RequiredMinimumReceive:   orDefault(sent.GetBfd().GetRequiredMinimumReceive(), 1000000),
			DetectionMultiplier:      orDefault(sent.GetBfd().GetDetectionMultiplier(), 3),
		},
		State: &gobgpapi.PeerState{
			NeighborAddress: sent.GetConf().GetNeighborAddress(),
			SessionState:    gobgpapi.PeerState_SESSION_STATE_ESTABLISHED,
			Messages:        &gobgpapi.Messages{},
			// Computed by the daemon from the resolved config and reported on
			// every ListPeer, which is what makes TCP-MD5 presence comparable at
			// all - the password itself comes back redacted. Read from the
			// ORIGINAL request, not from conf: the redaction above has already
			// blanked the clone.
			AuthPasswordSet: sent.GetConf().GetAuthPassword() != "",
		},
	}
	return echo
}

func orDefault(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}

// gobgpdEchoPeerGroup returns what ListPeerGroup would report.
//
// Note what it does NOT do: apply BFD defaults. addPeerGroup calls no defaulting
// function at all, so a peer group's BFD block comes back exactly as sent -
// unlike a peer's. Comparing the two with one scheme is why this needs its own
// helper rather than reusing gobgpdEcho.
func gobgpdEchoPeerGroup(sent *gobgpapi.PeerGroup) *gobgpapi.PeerGroup {
	conf := proto.CloneOf(sent.GetConf())
	if conf != nil {
		// PeerGroupConf keeps plain values - a peer group is only ever a source
		// of values and never inherits, so it gained no explicit presence.
		conf.AuthPassword = "" // redacted, same as ListPeerGroup
	}
	return &gobgpapi.PeerGroup{
		Conf:     conf,
		AfiSafis: sent.GetAfiSafis(),
		Bfd:      sent.GetBfd(),
		ApplyPolicy: &gobgpapi.ApplyPolicy{
			ImportPolicy: &gobgpapi.PolicyAssignment{Direction: gobgpapi.PolicyDirection_POLICY_DIRECTION_IMPORT},
			ExportPolicy: &gobgpapi.PolicyAssignment{Direction: gobgpapi.PolicyDirection_POLICY_DIRECTION_EXPORT},
		},
		Timers: &gobgpapi.Timers{
			Config: &gobgpapi.TimersConfig{
				ConnectRetry:      sent.GetTimers().GetConfig().GetConnectRetry(),
				HoldTime:          sent.GetTimers().GetConfig().GetHoldTime(),
				KeepaliveInterval: sent.GetTimers().GetConfig().GetKeepaliveInterval(),
			},
		},
		Transport: &gobgpapi.Transport{
			LocalAddress:  "invalid IP",
			PassiveMode:   sent.GetTransport().GetPassiveMode(),
			BindInterface: sent.GetTransport().GetBindInterface(),
			IpTos:         sent.GetTransport().GetIpTos(),
		},
	}
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
			// The case that churns if BFD defaults are applied only to a
			// non-empty block: the CR sends nothing, gobgpd returns
			// 3784/3/1e6/1e6. Every neighbor in a cluster that does not use BFD
			// at all looks like this.
			name: "no BFD block at all",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.9", PeerAsn: 64513},
			},
		},
		{
			name: "BFD enabled with CRD defaults filled in",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.10", PeerAsn: 64513},
				BFD: &bgpv1.BFD{
					Enabled: ptr(true), Port: 3784,
					DesiredMinimumTxInterval: 1000000,
					RequiredMinimumReceive:   1000000,
					DetectionMultiplier:      3,
				},
			},
		},
		{
			// Opt-out from a peer group that has BFD on. The API server fills the
			// other four fields as soon as the block exists, so this is a
			// non-empty block that disables rather than an absent one.
			name: "BFD explicitly disabled",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.11", PeerAsn: 64513},
				BFD: &bgpv1.BFD{
					Enabled: ptr(false), Port: 3784,
					DesiredMinimumTxInterval: 1000000,
					RequiredMinimumReceive:   1000000,
					DetectionMultiplier:      3,
				},
			},
		},
		{
			name: "BFD at the fastest profile the CRD allows",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.12", PeerAsn: 64513},
				BFD: &bgpv1.BFD{
					Enabled: ptr(true), Port: 3784,
					DesiredMinimumTxInterval: 300000,
					RequiredMinimumReceive:   300000,
					DetectionMultiplier:      3,
				},
			},
		},
		{
			// The PeerConf fields that had no CRD surface until they were wired.
			// Each is compared, so each is a fresh chance to churn - measured
			// against a live gobgpd v1.3.0, ListPeer echoes all six back as
			// sent. send_community is both sent and compared as of v1.3.1, where
			// it gained explicit presence and started round-tripping; it is
			// covered by sendCommunityEqual rather than here.
			name: "every previously-unexposed PeerConf field set",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{
					NeighborAddress:      "10.0.0.20",
					PeerAsn:              64513,
					AllowOwnAsn:          proto.Uint32(3),
					ReplacePeerAsn:       proto.Bool(true),
					AllowAspathLoopLocal: proto.Bool(true),
					RemovePrivate:        "replace",
					RouteFlapDamping:     true,
					SendSoftwareVersion:  proto.Bool(true),
					SendCommunity:        "both",
				},
			},
		},
		{
			// "standard" is ordinal 0. Under the pre-v1.3.1 proto this was
			// indistinguishable from unset and could not be expressed at all.
			name: "sendCommunity standard, the zero ordinal",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{
					NeighborAddress: "10.0.0.22", PeerAsn: 64513,
					SendCommunity: "standard",
				},
			},
		},
		{
			// The same fields left at their zero values. gobgpd echoes zero for
			// each, so this is the case that would break if any of them were
			// defaulted server-side the way connectRetry and localAsn are.
			name: "previously-unexposed PeerConf fields all omitted",
			neighbor: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.0.0.21", PeerAsn: 64513},
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
					AuthPassword:    proto.String("correct horse battery staple"),
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
			//
			// The password has to be the resolved one, not "": reconcileNeighbors
			// resolves it from the CR before converting, so passing "" here builds
			// a `current` that disagrees with what the reconciler will send and the
			// TCP-MD5 presence check reports drift on an unchanged neighbor.
			sent := r.crdToAPINeighborWithPassword(&tc.neighbor, tc.neighbor.Config.AuthPassword)
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
	fake := &fakeGoBGP{peers: []*gobgpapi.Peer{gobgpdEcho(r.crdToAPINeighborWithPassword(&existing, nil))}}

	require.NoError(t, r.reconcileNeighbors(context.Background(), fake, cfg, map[string]*gobgpapi.PeerGroup{}, logf.Log))
	assert.Equal(t, 1, fake.updatePeer, "a changed hold time must produce exactly one UpdatePeer")
}

func ptr[T any](v T) *T { return &v }

// TestPeerGroupConfigEqual_BFDNotDefaulted covers the peer group path, which
// differs from the peer one: addPeerGroup runs no defaulting, so ListPeerGroup
// echoes the BFD block as sent.
//
// Honest about what this does and does not prove. It asserts the peer group
// comparator is idempotent, which is what matters. It does *not* detect the peer
// scheme being applied here by mistake - bfdEqual defaults both sides, so nil
// against nil compares equal either way. The reason to model the daemon
// accurately anyway is that it stops being equivalent the moment the fork starts
// defaulting peer groups.
func TestPeerGroupConfigEqual_BFDNotDefaulted(t *testing.T) {
	r := &BGPConfigurationReconciler{Log: logf.Log}

	for _, tc := range []struct {
		name  string
		group bgpv1.PeerGroup
	}{
		{
			name: "no BFD block",
			group: bgpv1.PeerGroup{
				Config: bgpv1.PeerGroupConfig{PeerGroupName: "g1", PeerAsn: 64513},
			},
		},
		{
			name: "BFD enabled",
			group: bgpv1.PeerGroup{
				Config: bgpv1.PeerGroupConfig{PeerGroupName: "g2", PeerAsn: 64513},
				BFD: &bgpv1.BFD{
					Enabled: ptr(true), Port: 3784,
					DesiredMinimumTxInterval: 1000000,
					RequiredMinimumReceive:   1000000,
					DetectionMultiplier:      3,
				},
			},
		},
		{
			name: "AS path options set",
			group: bgpv1.PeerGroup{
				Config: bgpv1.PeerGroupConfig{
					PeerGroupName: "g3", PeerAsn: 64513,
					AllowOwnAsn: 2, ReplacePeerAsn: true, AllowAspathLoopLocal: true,
				},
			},
		},
		{
			name: "MD5 password, which ListPeerGroup redacts",
			group: bgpv1.PeerGroup{
				Config: bgpv1.PeerGroupConfig{
					PeerGroupName: "g4", PeerAsn: 64513, AuthPassword: "s3cret",
				},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := r.crdToAPIPeerGroupWithPassword(&tc.group, tc.group.Config.AuthPassword)
			require.NotNil(t, sent)
			assert.True(t, peerGroupConfigEqual(sent, gobgpdEchoPeerGroup(sent)),
				"peer group compares unequal against what ListPeerGroup returns, so UpdatePeerGroup fires every reconcile")
		})
	}
}

// TestBfdEqual_DefaultingIsPerObject pins the asymmetry directly, so a future
// change that unifies the two schemes fails here with an explanation rather
// than as mysterious churn on a cluster.
func TestBfdEqual_DefaultingIsPerObject(t *testing.T) {
	sent := (*gobgpapi.BfdPeerConfig)(nil)
	peerEcho := &gobgpapi.BfdPeerConfig{
		Port: 3784, DetectionMultiplier: 3,
		DesiredMinimumTxInterval: 1000000, RequiredMinimumReceive: 1000000,
	}

	assert.True(t, bfdEqual(sent, peerEcho, true),
		"a peer sending no BFD must match gobgpd's defaulted echo")
	assert.False(t, bfdEqual(sent, peerEcho, false),
		"without defaulting the same pair is unequal - which is why the flag exists")
	assert.True(t, bfdEqual(sent, nil, false),
		"a peer group sending no BFD must match the empty block ListPeerGroup returns")
	assert.True(t, bfdEqual(sent, nil, true),
		"defaulting both sides of an empty pair is a no-op - recorded so the peer group flag is not mistaken for load-bearing")

	enabled := &gobgpapi.BfdPeerConfig{Enabled: true, Port: 3784, DetectionMultiplier: 3,
		DesiredMinimumTxInterval: 1000000, RequiredMinimumReceive: 1000000}
	assert.False(t, bfdEqual(enabled, peerEcho, true),
		"enabling BFD must still be detected as a change")
}

// TestPeerConfigEqual_AgainstProductionPayload pins the exact pair that churned
// on a live cluster, captured rather than imagined.
//
// `desired` is built by the real converter from the BGPConfiguration that was
// deployed; `current` is the verbatim `gobgp -j neighbor` output from the
// gobgpd it was talking to. Every field here is transcribed from that capture.
//
// This exists because the synthetic fixtures above all passed while production
// fired UpdatePeer on every reconcile. What the CR omits is what matters, and
// this CR omits three things the fixtures happened to set: connectRetry,
// localAsn, and any transport block. gobgpd defaults all three and echoes the
// defaulted value, so each one was permanently unequal.
func TestPeerConfigEqual_AgainstProductionPayload(t *testing.T) {
	r := &BGPConfigurationReconciler{}

	// Exactly the CR that was deployed - note what it does NOT set.
	crd := &bgpv1.Neighbor{
		Config: bgpv1.NeighborConfig{
			NeighborAddress: "2001:470:b8f3:251::1",
			PeerAsn:         64514,
			Description:     proto.String("Gateway router on subnet-251 (IPv6)"),
		},
		AfiSafis: []bgpv1.AfiSafi{
			{Family: "ipv4-unicast", Enabled: true},
			{Family: "ipv6-unicast", Enabled: true},
		},
		Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{HoldTime: 90, KeepaliveInterval: 30}},
	}
	desired := r.crdToAPINeighborWithPassword(crd, nil)

	// Verbatim from `gobgp --target unix://... -j neighbor 2001:470:b8f3:251::1`
	// against gobgpd v1.3.0 (commit 8b99965), session established.
	current := &gobgpapi.Peer{
		Conf: &gobgpapi.PeerConf{
			NeighborAddress: "2001:470:b8f3:251::1",
			PeerAsn:         64514,
			// Defaulted from global.asn; the CR never set it.
			LocalAsn:    proto.Uint32(64515),
			Description: proto.String("Gateway router on subnet-251 (IPv6)"),
			Type:        gobgpapi.PeerType_PEER_TYPE_EXTERNAL,
		},
		// Resolved once established; the CR has no transport block at all.
		Transport: &gobgpapi.Transport{LocalAddress: "2001:470:b8f3:251:be24:11ff:fe9b:a72b"},
		Timers: &gobgpapi.Timers{
			Config: &gobgpapi.TimersConfig{
				ConnectRetry: 120, HoldTime: 90, KeepaliveInterval: 30,
				IdleHoldTimeAfterReset: 30,
			},
			State: &gobgpapi.TimersState{KeepaliveInterval: 3, NegotiatedHoldTime: 9},
		},
		GracefulRestart: &gobgpapi.GracefulRestart{},
		RouteReflector:  &gobgpapi.RouteReflector{RouteReflectorClusterId: "invalid IP"},
		EbgpMultihop:    &gobgpapi.EbgpMultihop{},
		ApplyPolicy: &gobgpapi.ApplyPolicy{
			ImportPolicy: &gobgpapi.PolicyAssignment{Direction: gobgpapi.PolicyDirection_POLICY_DIRECTION_IMPORT},
			ExportPolicy: &gobgpapi.PolicyAssignment{Direction: gobgpapi.PolicyDirection_POLICY_DIRECTION_EXPORT},
		},
		Bfd: &gobgpapi.BfdPeerConfig{
			Port: 3784, DesiredMinimumTxInterval: 1000000,
			RequiredMinimumReceive: 1000000, DetectionMultiplier: 3,
		},
		AfiSafis: []*gobgpapi.AfiSafi{
			{Config: &gobgpapi.AfiSafiConfig{Family: &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP, Safi: gobgpapi.Family_SAFI_UNICAST}, Enabled: true}},
			{Config: &gobgpapi.AfiSafiConfig{Family: &gobgpapi.Family{Afi: gobgpapi.Family_AFI_IP6, Safi: gobgpapi.Family_SAFI_UNICAST}, Enabled: true}},
		},
	}

	assert.True(t, peerConfigEqual(desired, current),
		"this exact pair fired UpdatePeer on every reconcile in production")
}

// --- global drift ------------------------------------------------------------

// fakeGlobalGoBGP reports a running BGP server whose Global the caller controls,
// so drift can be simulated without a daemon.
type fakeGlobalGoBGP struct {
	gobgpapi.GoBgpServiceClient
	global   *gobgpapi.Global
	startBgp int
}

func (f *fakeGlobalGoBGP) GetBgp(_ context.Context, _ *gobgpapi.GetBgpRequest, _ ...grpc.CallOption) (*gobgpapi.GetBgpResponse, error) {
	return &gobgpapi.GetBgpResponse{Global: f.global}, nil
}

func (f *fakeGlobalGoBGP) StartBgp(_ context.Context, _ *gobgpapi.StartBgpRequest, _ ...grpc.CallOption) (*gobgpapi.StartBgpResponse, error) {
	f.startBgp++
	return &gobgpapi.StartBgpResponse{}, nil
}

// drainEvents returns every event queued on a fake recorder. reconcileGlobal
// emits RouterIDResolved as well, so a test that reads only the first event is
// asserting on emission order it has no reason to rely on.
func drainEvents(rec *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func eventMatching(events []string, substr string) (string, bool) {
	for _, e := range events {
		if strings.Contains(e, substr) {
			return e, true
		}
	}
	return "", false
}

// Secondary global drift must NOT abort reconcileGlobal.
//
// reconcileGlobal is first in the chain, so returning an error here stops
// neighbors, peer groups, policies and netlink from being reconciled at all. An
// earlier version did exactly that, which meant one edit to a secondary global
// setting froze every other piece of configuration on the node until the pod
// happened to restart. The drift is reported and the reconcile continues.
func TestReconcileGlobal_SecondaryDriftDoesNotAbortTheChain(t *testing.T) {
	cfg := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "drift", Namespace: "purelb"},
		Spec: bgpv1.BGPConfigurationSpec{Global: bgpv1.GlobalSpec{
			ASN:              64512,
			RouterID:         "10.0.0.1",
			UseMultiplePaths: true,
			BindToDevice:     "eth1",
		}},
	}
	// Running daemon: same identity, every secondary setting different.
	fake := &fakeGlobalGoBGP{global: &gobgpapi.Global{
		Asn:              64512,
		RouterId:         "10.0.0.1",
		UseMultiplePaths: false,
		BindToDevice:     "",
	}}

	// Sized well above the number of events: record.NewFakeRecorder blocks on a
	// full channel rather than dropping, so an undersized one deadlocks the test
	// instead of failing it.
	rec := record.NewFakeRecorder(64)
	r := &BGPConfigurationReconciler{Log: logf.Log, Recorder: rec, NodeName: "node-1"}

	err := r.reconcileGlobal(context.Background(), fake, cfg, logf.Log)
	require.NoError(t, err, "secondary global drift must not return an error: it would abort the reconcile chain")
	assert.Zero(t, fake.startBgp, "StartBgp must not be called for a server that is already running")

	got, ok := eventMatching(drainEvents(rec), "GlobalRestartRequired")
	require.True(t, ok, "expected a GlobalRestartRequired event")
	assert.Contains(t, got, "useMultiplePaths")
	assert.Contains(t, got, "bindToDevice")
}

// Identity drift is different: it DOES stop, because continuing would reconcile
// neighbors against a speaker that is not the one the CR describes.
func TestReconcileGlobal_IdentityDriftStops(t *testing.T) {
	cfg := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "drift", Namespace: "purelb"},
		Spec:       bgpv1.BGPConfigurationSpec{Global: bgpv1.GlobalSpec{ASN: 64512, RouterID: "10.0.0.1"}},
	}
	fake := &fakeGlobalGoBGP{global: &gobgpapi.Global{Asn: 64599, RouterId: "10.0.0.1"}}
	r := &BGPConfigurationReconciler{Log: logf.Log, Recorder: record.NewFakeRecorder(64), NodeName: "node-1"}

	err := r.reconcileGlobal(context.Background(), fake, cfg, logf.Log)
	require.Error(t, err, "an ASN change must stop the reconcile")
	assert.Contains(t, err.Error(), "global identity change")
}

// A CR that states nothing for the defaulted fields must not report drift against
// the daemon's defaults. This is the shape that would churn if the guards were
// dropped: listenAddresses becomes ["0.0.0.0", "::"] and families becomes every
// supported family, neither of which the CR asked for.
func TestReconcileGlobal_DefaultedFieldsDoNotDrift(t *testing.T) {
	cfg := &bgpv1.BGPConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "drift", Namespace: "purelb"},
		Spec:       bgpv1.BGPConfigurationSpec{Global: bgpv1.GlobalSpec{ASN: 64512, RouterID: "10.0.0.1"}},
	}
	fake := &fakeGlobalGoBGP{global: &gobgpapi.Global{
		Asn:             64512,
		RouterId:        "10.0.0.1",
		ListenPort:      179,
		ListenAddresses: []string{"0.0.0.0", "::"},
		Families:        []uint32{0, 1, 9, 24},
	}}
	rec := record.NewFakeRecorder(64)
	r := &BGPConfigurationReconciler{Log: logf.Log, Recorder: rec, NodeName: "node-1"}

	require.NoError(t, r.reconcileGlobal(context.Background(), fake, cfg, logf.Log))
	if e, ok := eventMatching(drainEvents(rec), "GlobalRestartRequired"); ok {
		t.Fatalf("no drift expected for daemon-defaulted fields, got: %s", e)
	}
}
