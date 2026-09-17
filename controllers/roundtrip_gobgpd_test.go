//go:build gobgpd

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

// Round-trip tests against a REAL gobgpd.
//
// This is the guard the reconcile-idempotency tests cannot be. Those compare
// our converters against gobgpdEcho(), a fake - and a fake encodes what its
// author believed the daemon does. When that belief is wrong the fake and the
// comparator agree with each other and both are wrong: connectRetry (0 vs a
// defaulted 120) and localAsn (0 vs the global ASN) both churned in production
// for exactly that reason, with 13 green subtests.
//
// Here the only Go under test is production code - the real converters and the
// real comparators - and the other side is the actual daemon at the pinned
// commit. There is no place left for a belief to hide.
//
// Run:
//
//	docker run -d --name gobgpd-rt -p 50051:50051 --entrypoint gobgpd \
//	    <k8gobgp image> --api-hosts 0.0.0.0:50051 --pprof-disable
//	go test -tags=gobgpd ./controllers/ -run TestRoundTrip -v
//
// GOBGPD_ADDR overrides the default 127.0.0.1:50051.
package controllers

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

func rtClient(t *testing.T) (gobgpapi.GoBgpServiceClient, context.Context, func()) {
	t.Helper()
	addr := os.Getenv("GOBGPD_ADDR")
	if addr == "" {
		addr = "127.0.0.1:50051"
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	c := gobgpapi.NewGoBgpServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

	// StopBgp is not a reset - it ends the BGP server for the life of the
	// process and every later StartBgp fails - so this starts it only if it is
	// not already running.
	if resp, err := c.GetBgp(ctx, &gobgpapi.GetBgpRequest{}); err != nil || resp.GetGlobal().GetAsn() == 0 {
		_, err = c.StartBgp(ctx, &gobgpapi.StartBgpRequest{
			Global: &gobgpapi.Global{Asn: 64512, RouterId: "10.0.0.1"},
		})
		require.NoError(t, err, "StartBgp")
	}
	return c, ctx, func() { cancel(); conn.Close() }
}

// --- neighbors ----------------------------------------------------------------

// TestRoundTripNeighbor is the important one. It sends a neighbor with every
// CRD field this controller can express, reads it back, and asserts the real
// peerConfigEqual says they match. A false here is a live UpdatePeer on every
// reconcile for any CR shaped like the case.
func TestRoundTripNeighbor(t *testing.T) {
	c, ctx, done := rtClient(t)
	defer done()

	r := &BGPConfigurationReconciler{}
	cases := []struct {
		name string
		crd  bgpv1.Neighbor
	}{
		{
			// The common case, and the one that churned: a CR that sets very
			// little, against a daemon that defaults a great deal.
			name: "minimal - everything optional omitted",
			crd: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.90.0.1", PeerAsn: 64513},
			},
		},
		{
			name: "timers set but connectRetry omitted (the v1.3.0 churn)",
			crd: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.90.0.2", PeerAsn: 64513},
				Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{HoldTime: 90, KeepaliveInterval: 30}},
			},
		},
		{
			name: "localAsn omitted, so gobgpd defaults it to the global ASN",
			crd: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.90.0.3", PeerAsn: 64513},
			},
		},
		{
			name: "every field this CRD can express",
			crd: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{
					NeighborAddress: "10.90.0.4", PeerAsn: 64513, LocalAsn: 64512,
					Description: "everything", AdminDown: false,
					AllowOwnAsn: 3, ReplacePeerAsn: true, AllowAspathLoopLocal: true,
					RemovePrivate: "replace", RouteFlapDamping: true,
					SendSoftwareVersion: true, SendCommunity: "both",
				},
				AfiSafis: []bgpv1.AfiSafi{
					{Family: "ipv4-unicast", Enabled: true,
						PrefixLimit: &bgpv1.PrefixLimit{MaxPrefixes: 1000, ShutdownThresholdPct: 80},
						AddPaths:    &bgpv1.AddPaths{Receive: true, SendMax: 4}},
					{Family: "ipv6-unicast", Enabled: true},
				},
				Timers:          &bgpv1.Timers{Config: bgpv1.TimersConfig{ConnectRetry: 120, HoldTime: 90, KeepaliveInterval: 30}},
				Transport:       &bgpv1.Transport{PassiveMode: true, IpTos: 16},
				GracefulRestart: &bgpv1.GracefulRestart{Enabled: true, RestartTime: 120},
				EbgpMultihop:    &bgpv1.EbgpMultihop{Enabled: true, MultihopTtl: 4},
				BFD: &bgpv1.BFD{
					Enabled: ptr(true), Port: 3784,
					DesiredMinimumTxInterval: 1000000, RequiredMinimumReceive: 1000000,
					DetectionMultiplier: 3,
				},
			},
		},
		{
			name: "sendCommunity standard, the zero ordinal",
			crd: bgpv1.Neighbor{
				Config: bgpv1.NeighborConfig{NeighborAddress: "10.90.0.5", PeerAsn: 64513, SendCommunity: "standard"},
			},
		},
		{
			name: "route reflector client without an explicit cluster ID",
			crd: bgpv1.Neighbor{
				Config:         bgpv1.NeighborConfig{NeighborAddress: "10.90.0.6", PeerAsn: 64512},
				RouteReflector: &bgpv1.RouteReflector{RouteReflectorClient: true},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := r.crdToAPINeighborWithPassword(&tc.crd, "")
			// Delete first: AddPeer errors on an existing peer, and a suite that
			// only passes against a pristine daemon fails confusingly on the
			// second run.
			_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: tc.crd.Config.NeighborAddress})
			_, err := c.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired})
			require.NoError(t, err, "AddPeer rejected the config our converter produced")

			stream, err := c.ListPeer(ctx, &gobgpapi.ListPeerRequest{Address: tc.crd.Config.NeighborAddress})
			require.NoError(t, err)
			resp, err := stream.Recv()
			require.NoError(t, err, "ListPeer returned no peer")
			current := resp.GetPeer()

			if !peerConfigEqual(desired, current) {
				t.Errorf("peerConfigEqual is FALSE on a freshly-added peer: every reconcile "+
					"would fire UpdatePeer for this config.\n\nsent:\n%s\ngot back:\n%s",
					mustJSON(desired.GetConf()), mustJSON(current.GetConf()))
				reportPeerDiff(t, desired, current)
			}
		})
	}
}

// reportPeerDiff names the sub-message whose comparator disagrees, so a failure
// says which field to look at rather than just that something moved.
func reportPeerDiff(t *testing.T, desired, current *gobgpapi.Peer) {
	t.Helper()
	dc, cc := desired.GetConf(), current.GetConf()
	type check struct {
		name string
		ok   bool
	}
	for _, c := range []check{
		{"Conf.PeerAsn", dc.GetPeerAsn() == cc.GetPeerAsn()},
		{"Conf.LocalAsn", dc.GetLocalAsn() == 0 || dc.GetLocalAsn() == cc.GetLocalAsn()},
		{"Conf.Description", dc.GetDescription() == cc.GetDescription()},
		{"Conf.PeerGroup", dc.GetPeerGroup() == cc.GetPeerGroup()},
		{"Conf.AdminDown", dc.GetAdminDown() == cc.GetAdminDown()},
		{"Conf.NeighborInterface", dc.GetNeighborInterface() == cc.GetNeighborInterface()},
		{"Conf.Vrf", dc.GetVrf() == cc.GetVrf()},
		{"Conf.AllowOwnAsn", dc.GetAllowOwnAsn() == cc.GetAllowOwnAsn()},
		{"Conf.ReplacePeerAsn", dc.GetReplacePeerAsn() == cc.GetReplacePeerAsn()},
		{"Conf.AllowAspathLoopLocal", dc.GetAllowAspathLoopLocal() == cc.GetAllowAspathLoopLocal()},
		{"Conf.RemovePrivate", dc.GetRemovePrivate() == cc.GetRemovePrivate()},
		{"Conf.RouteFlapDamping", dc.GetRouteFlapDamping() == cc.GetRouteFlapDamping()},
		{"Conf.SendSoftwareVersion", dc.GetSendSoftwareVersion() == cc.GetSendSoftwareVersion()},
		{"Conf.SendCommunity", sendCommunityEqual(dc.SendCommunity, cc.SendCommunity)},
		{"AfiSafis", afiSafisConfigEqual(desired.GetAfiSafis(), current.GetAfiSafis())},
		{"ApplyPolicy", applyPolicyEqual(desired.GetApplyPolicy(), current.GetApplyPolicy())},
		{"Timers", timersConfigEqual(desired.GetTimers(), current.GetTimers())},
		{"Transport", transportConfigEqual(desired.GetTransport(), current.GetTransport())},
		{"GracefulRestart", gracefulRestartEqual(desired.GetGracefulRestart(), current.GetGracefulRestart())},
		{"RouteReflector", routeReflectorEqual(desired.GetRouteReflector(), current.GetRouteReflector())},
		{"EbgpMultihop", ebgpMultihopEqual(desired.GetEbgpMultihop(), current.GetEbgpMultihop())},
		{"Bfd", bfdEqual(desired.GetBfd(), current.GetBfd(), true)},
	} {
		if !c.ok {
			t.Errorf("  --> %s does not round-trip", c.name)
		}
	}
}

// --- peer groups --------------------------------------------------------------

// Peer groups are defaulted differently from peers - addPeerGroup runs no
// defaulting function - and an UpdatePeerGroup loops updateNeighbor over every
// member, so churn here is more expensive than churn on a single peer.
func TestRoundTripPeerGroup(t *testing.T) {
	c, ctx, done := rtClient(t)
	defer done()

	r := &BGPConfigurationReconciler{}
	cases := []struct {
		name string
		crd  bgpv1.PeerGroup
	}{
		{
			name: "minimal",
			crd:  bgpv1.PeerGroup{Config: bgpv1.PeerGroupConfig{PeerGroupName: "rt-min", PeerAsn: 64513}},
		},
		{
			name: "every field",
			crd: bgpv1.PeerGroup{
				Config: bgpv1.PeerGroupConfig{
					PeerGroupName: "rt-full", PeerAsn: 64513, LocalAsn: 64512,
					Description: "everything", AllowOwnAsn: 3, ReplacePeerAsn: true,
					AllowAspathLoopLocal: true, RemovePrivate: "all",
					RouteFlapDamping: true, SendSoftwareVersion: true, SendCommunity: "none",
				},
				AfiSafis:  []bgpv1.AfiSafi{{Family: "ipv4-unicast", Enabled: true}},
				Timers:    &bgpv1.Timers{Config: bgpv1.TimersConfig{HoldTime: 90, KeepaliveInterval: 30}},
				Transport: &bgpv1.Transport{PassiveMode: true},
				BFD:       &bgpv1.BFD{Enabled: ptr(true)},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			desired := r.crdToAPIPeerGroupWithPassword(&tc.crd, "")
			_, _ = c.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: tc.crd.Config.PeerGroupName})
			_, err := c.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{PeerGroup: desired})
			require.NoError(t, err, "AddPeerGroup rejected our config")

			stream, err := c.ListPeerGroup(ctx, &gobgpapi.ListPeerGroupRequest{PeerGroupName: tc.crd.Config.PeerGroupName})
			require.NoError(t, err)
			resp, err := stream.Recv()
			require.NoError(t, err)
			current := resp.GetPeerGroup()

			if !peerGroupConfigEqual(desired, current) {
				t.Errorf("peerGroupConfigEqual is FALSE on a freshly-added group: every reconcile "+
					"would fire UpdatePeerGroup, which loops updateNeighbor over every member."+
					"\n\nsent:\n%s\ngot back:\n%s", mustJSON(desired.GetConf()), mustJSON(current.GetConf()))
				for _, ck := range []struct {
					name string
					ok   bool
				}{
					{"AfiSafis", afiSafisConfigEqual(desired.GetAfiSafis(), current.GetAfiSafis())},
					{"ApplyPolicy", applyPolicyEqual(desired.GetApplyPolicy(), current.GetApplyPolicy())},
					{"Timers", timersConfigEqual(desired.GetTimers(), current.GetTimers())},
					{"Transport", transportConfigEqual(desired.GetTransport(), current.GetTransport())},
					{"GracefulRestart", gracefulRestartEqual(desired.GetGracefulRestart(), current.GetGracefulRestart())},
					{"Bfd (not defaulted)", bfdEqual(desired.GetBfd(), current.GetBfd(), false)},
					{"SendCommunity", sendCommunityEqual(desired.GetConf().SendCommunity, current.GetConf().SendCommunity)},
				} {
					if !ck.ok {
						t.Errorf("  --> %s does not round-trip", ck.name)
					}
				}
				t.Logf("desired bfd: %s", mustJSON(desired.GetBfd()))
				t.Logf("current bfd: %s", mustJSON(current.GetBfd()))
			}
		})
	}
}

// --- defined sets -------------------------------------------------------------

// reconcileDefinedSets drives AddDefinedSet(Replace: true) off definedSetEqual,
// so a false here rewrites every defined set on every reconcile.
func TestRoundTripDefinedSet(t *testing.T) {
	c, ctx, done := rtClient(t)
	defer done()

	cases := []bgpv1.DefinedSet{
		{Name: "rt-prefix", Type: "prefix", Prefixes: []bgpv1.Prefix{
			{IpPrefix: "10.0.0.0/8", MaskLengthMin: 8, MaskLengthMax: 24},
			{IpPrefix: "192.168.0.0/16"},
		}},
		{Name: "rt-neighbor", Type: "neighbor", List: []string{"10.0.0.1/32"}},
		{Name: "rt-community", Type: "community", List: []string{"65000:100"}},
		{Name: "rt-aspath", Type: "as-path", List: []string{"^65000"}},
	}
	for _, crd := range cases {
		t.Run(crd.Name, func(t *testing.T) {
			desired := crdToAPIDefinedSet(&crd)
			_, err := c.AddDefinedSet(ctx, &gobgpapi.AddDefinedSetRequest{DefinedSet: desired, Replace: true})
			require.NoError(t, err, "AddDefinedSet rejected our config")

			stream, err := c.ListDefinedSet(ctx, &gobgpapi.ListDefinedSetRequest{
				DefinedType: desired.GetDefinedType(), Name: crd.Name,
			})
			require.NoError(t, err)
			var current *gobgpapi.DefinedSet
			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				if resp.GetDefinedSet().GetName() == crd.Name {
					current = resp.GetDefinedSet()
				}
			}
			require.NotNil(t, current, "ListDefinedSet did not return %q", crd.Name)

			if !definedSetEqual(desired, current) {
				t.Errorf("definedSetEqual is FALSE on a freshly-added set: every reconcile would "+
					"rewrite it.\n\nsent:\n%s\ngot back:\n%s", mustJSON(desired), mustJSON(current))
			}
		})
	}
}

func mustJSON(m proto.Message) string {
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		return fmt.Sprintf("%+v", m)
	}
	return string(b)
}
