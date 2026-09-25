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
					Description: ptr("everything"), AdminDown: false,
					AllowOwnAsn: ptr(uint32(3)), ReplacePeerAsn: ptr(true), AllowAspathLoopLocal: ptr(true),
					RemovePrivate: "replace", RouteFlapDamping: true,
					SendSoftwareVersion: ptr(true), SendCommunity: "both",
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
			desired := r.crdToAPINeighborWithPassword(&tc.crd, nil)
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
		// These mirror peerConfigEqual's guards exactly. A check that is stricter
		// than the comparator reports diffs the reconciler will not act on, which
		// reads as a harness failure for correct behaviour.
		{"Conf.PeerAsn", dc.GetPeerGroup() != "" || dc.GetPeerAsn() == cc.GetPeerAsn()},
		{"Conf.LocalAsn", dc.GetLocalAsn() == 0 || dc.GetLocalAsn() == cc.GetLocalAsn()},
		{"Conf.Description", dc.GetDescription() == "" || dc.GetDescription() == cc.GetDescription()},
		{"Conf.PeerGroup", dc.GetPeerGroup() == cc.GetPeerGroup()},
		{"Conf.AdminDown", dc.GetAdminDown() == cc.GetAdminDown()},
		{"Conf.NeighborInterface", dc.GetNeighborInterface() == cc.GetNeighborInterface()},
		{"Conf.Vrf", dc.GetVrf() == cc.GetVrf()},
		{"Conf.AllowOwnAsn", dc.AllowOwnAsn == nil || dc.GetAllowOwnAsn() == cc.GetAllowOwnAsn()},
		{"Conf.ReplacePeerAsn", dc.ReplacePeerAsn == nil || dc.GetReplacePeerAsn() == cc.GetReplacePeerAsn()},
		{"Conf.AllowAspathLoopLocal", dc.AllowAspathLoopLocal == nil || dc.GetAllowAspathLoopLocal() == cc.GetAllowAspathLoopLocal()},
		{"Conf.RemovePrivate", dc.RemovePrivate == nil || dc.GetRemovePrivate() == cc.GetRemovePrivate()},
		{"Conf.SendSoftwareVersion", dc.SendSoftwareVersion == nil || dc.GetSendSoftwareVersion() == cc.GetSendSoftwareVersion()},
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

// --- peer-group inheritance ---------------------------------------------------

// TestRoundTripPeerGroupInheritance covers the cases where a neighbor is in a
// peer group, which is where presence semantics decide the outcome and where
// every peer-group defect in this engagement lived.
//
// The shapes here are deliberately the inheritance direction - group states a
// value, member omits it - because that is the direction no other test in this
// file exercises. A send path that claimed every field with an explicit zero
// would pass every other case in this suite and silently switch off inheritance
// for all of them.
func TestRoundTripPeerGroupInheritance(t *testing.T) {
	c, ctx, done := rtClient(t)
	defer done()
	r := &BGPConfigurationReconciler{}

	// Clear anything a previous run left behind. A peer group cannot be deleted
	// while it still has members, so the peers go first - otherwise DeletePeerGroup
	// fails silently and AddPeerGroup reports "can't overwrite the existing
	// peer-group", which looks like a daemon bug rather than leftover state.
	for i := 1; i <= 14; i++ {
		_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{
			Address: fmt.Sprintf("10.90.0.%d", i),
		})
	}

	const pg = "rt-inherit"
	// The group states values for everything a member might inherit.
	group := bgpv1.PeerGroup{Config: bgpv1.PeerGroupConfig{
		PeerGroupName: pg, PeerAsn: 64513, Description: "from-the-group",
		AllowOwnAsn: 3, ReplacePeerAsn: true, AllowAspathLoopLocal: true,
		RemovePrivate: "all", SendSoftwareVersion: true, SendCommunity: "none",
	},
		Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{HoldTime: 30, KeepaliveInterval: 10}},
	}
	_, _ = c.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: pg})
	_, err := c.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{
		PeerGroup: r.crdToAPIPeerGroupWithPassword(&group, ""),
	})
	require.NoError(t, err, "AddPeerGroup")

	// A second group that sets a password, for the opt-out case. Kept separate so
	// the other cases are not all authenticated.
	const pgWithPassword = "rt-inherit-md5"
	groupPw := bgpv1.PeerGroup{Config: bgpv1.PeerGroupConfig{
		PeerGroupName: pgWithPassword, PeerAsn: 64513, AuthPassword: "group-secret",
	}}
	_, _ = c.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: pgWithPassword})
	_, err = c.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{
		PeerGroup: r.crdToAPIPeerGroupWithPassword(&groupPw, "group-secret"),
	})
	require.NoError(t, err, "AddPeerGroup (md5)")

	// readBackIn adds the neighbor and returns what ListPeer reports. The password
	// is resolved the way reconcileNeighbors resolves it, so an explicitly empty
	// authPassword reaches the daemon as a non-nil empty string rather than as
	// absence - which is the whole point of the opt-out case.
	readBackIn := func(t *testing.T, group string, n bgpv1.Neighbor) (*gobgpapi.Peer, *gobgpapi.Peer) {
		t.Helper()
		desired := r.crdToAPINeighborWithPassword(&n, n.Config.AuthPassword)
		addr := n.Config.NeighborAddress
		_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: addr})
		_, err := c.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired})
		require.NoError(t, err, "AddPeer rejected our config")
		stream, err := c.ListPeer(ctx, &gobgpapi.ListPeerRequest{Address: addr})
		require.NoError(t, err)
		resp, err := stream.Recv()
		require.NoError(t, err)
		return desired, resp.GetPeer()
	}

	// readBack adds the neighbor and returns what ListPeer reports.
	readBack := func(t *testing.T, n bgpv1.Neighbor) (*gobgpapi.Peer, *gobgpapi.Peer) {
		t.Helper()
		desired := r.crdToAPINeighborWithPassword(&n, nil)
		addr := n.Config.NeighborAddress
		_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: addr})
		_, err := c.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired})
		require.NoError(t, err, "AddPeer rejected our config")
		stream, err := c.ListPeer(ctx, &gobgpapi.ListPeerRequest{Address: addr})
		require.NoError(t, err)
		resp, err := stream.Recv()
		require.NoError(t, err)
		return desired, resp.GetPeer()
	}

	t.Run("member omitting a field inherits the group's", func(t *testing.T) {
		desired, current := readBack(t, bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.1", PeerAsn: 64513, PeerGroup: pg,
		}})
		cc := current.GetConf()
		require.Equal(t, "from-the-group", cc.GetDescription(), "description must inherit")
		require.Equal(t, uint32(3), cc.GetAllowOwnAsn(), "allowOwnAsn must inherit")
		require.True(t, cc.GetReplacePeerAsn(), "replacePeerAsn must inherit")
		require.True(t, cc.GetAllowAspathLoopLocal(), "allowAspathLoopLocal must inherit")
		require.True(t, cc.GetSendSoftwareVersion(), "sendSoftwareVersion must inherit")
		require.Equal(t, gobgpapi.RemovePrivate_REMOVE_PRIVATE_ALL, cc.GetRemovePrivate(), "removePrivate must inherit")
		require.Equal(t, uint64(30), current.GetTimers().GetConfig().GetHoldTime(), "timers must inherit")

		// And the comparator must not churn on any of it.
		if !peerConfigEqual(desired, current) {
			reportPeerDiff(t, desired, current)
			t.Error("peerConfigEqual is FALSE for a grouped neighbor that inherits: " +
				"every reconcile would fire UpdatePeer for a config we did not set")
		}
	})

	t.Run("member stating its own value keeps it", func(t *testing.T) {
		desired, current := readBack(t, bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.2", PeerAsn: 64513, PeerGroup: pg,
			Description: ptr("mine-not-the-groups"), SendSoftwareVersion: ptr(true),
		}})
		cc := current.GetConf()
		// This is the v1.3.4/v1.3.5 field-presence fix. Before it, the group won.
		require.Equal(t, "mine-not-the-groups", cc.GetDescription(),
			"a member's own description must survive peer-group inheritance")
		if !peerConfigEqual(desired, current) {
			reportPeerDiff(t, desired, current)
			t.Error("peerConfigEqual is FALSE for a grouped neighbor stating its own fields")
		}
	})

	t.Run("member opts out of the group's TCP-MD5", func(t *testing.T) {
		// The reason description, authPassword and the as-path fields are pointers
		// in the CRD. A plain string cannot distinguish "said nothing, inherit the
		// group's password" from "said empty, do not authenticate", so before this
		// a grouped member could not decline its group's TCP-MD5 key at all.
		//
		// Asserted through PeerState.AuthPasswordSet rather than the password,
		// which ListPeer redacts.
		withPw := bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.8", PeerAsn: 64513, PeerGroup: pgWithPassword,
		}}
		_, current := readBackIn(t, pgWithPassword, withPw)
		require.True(t, current.GetState().GetAuthPasswordSet(),
			"omitting authPassword must inherit the group's")

		optOut := bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.9", PeerAsn: 64513, PeerGroup: pgWithPassword,
			AuthPassword: ptr(""),
		}}
		desired, current := readBackIn(t, pgWithPassword, optOut)
		require.False(t, current.GetState().GetAuthPasswordSet(),
			"authPassword: \"\" must opt out of the group's password, not inherit it")
		if !peerConfigEqual(desired, current) {
			reportPeerDiff(t, desired, current)
			t.Error("peerConfigEqual is FALSE for a member that opted out of its group's password")
		}
	})

	t.Run("group sets bfd, member omits it", func(t *testing.T) {
		// The PeerGroup CRD gained a bfd block in the v1.3.0 work, so this IS an
		// inheritance path. An earlier comment in peerConfigEqual claimed it was
		// not, and excluded bfd from the inheritance skip on that basis.
		const pgBfd = "rt-inherit-bfd"
		gb := bgpv1.PeerGroup{
			Config: bgpv1.PeerGroupConfig{PeerGroupName: pgBfd, PeerAsn: 64513},
			BFD:    &bgpv1.BFD{Enabled: ptr(true), DesiredMinimumTxInterval: 300000},
		}
		_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: "10.90.0.12"})
		_, _ = c.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: pgBfd})
		_, err := c.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{
			PeerGroup: r.crdToAPIPeerGroupWithPassword(&gb, ""),
		})
		require.NoError(t, err, "AddPeerGroup (bfd)")

		desired, current := readBackIn(t, pgBfd, bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.12", PeerAsn: 64513, PeerGroup: pgBfd,
		}})
		require.True(t, current.GetBfd().GetEnabled(), "member must inherit the group's BFD")
		if !peerConfigEqual(desired, current) {
			reportPeerDiff(t, desired, current)
			t.Error("peerConfigEqual is FALSE for a grouped neighbor that inherits BFD: " +
				"every reconcile would fire UpdatePeer for a block the CR did not set")
		}
	})

	t.Run("group sets gracefulRestart, member omits it", func(t *testing.T) {
		// Verified against v1.3.5 that a group's graceful restart reaches a member
		// that states none, and that editing the group reaches members already in
		// it - which was not true before v1.3.5, where SetDefaultNeighborConfigValues
		// returned early once a member had resolved.
		const pgGR = "rt-inherit-gr"
		gg := bgpv1.PeerGroup{
			Config:          bgpv1.PeerGroupConfig{PeerGroupName: pgGR, PeerAsn: 64513},
			GracefulRestart: &bgpv1.GracefulRestart{Enabled: true, RestartTime: 120, StaleRoutesTime: 300},
		}
		_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: "10.90.0.13"})
		_, _ = c.DeletePeerGroup(ctx, &gobgpapi.DeletePeerGroupRequest{Name: pgGR})
		_, err := c.AddPeerGroup(ctx, &gobgpapi.AddPeerGroupRequest{
			PeerGroup: r.crdToAPIPeerGroupWithPassword(&gg, ""),
		})
		require.NoError(t, err, "AddPeerGroup (gr)")

		desired, current := readBackIn(t, pgGR, bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.13", PeerAsn: 64513, PeerGroup: pgGR,
		}})
		got := current.GetGracefulRestart()
		require.True(t, got.GetEnabled(), "member must inherit the group's graceful restart")
		require.Equal(t, uint32(120), got.GetRestartTime(), "restartTime must inherit")
		require.Equal(t, uint32(300), got.GetStaleRoutesTime(), "staleRoutesTime must inherit")
		if !peerConfigEqual(desired, current) {
			reportPeerDiff(t, desired, current)
			t.Error("peerConfigEqual is FALSE for a grouped neighbor that inherits graceful restart")
		}
	})

	t.Run("staleRoutesTime round-trips on a peer that states it", func(t *testing.T) {
		desired, current := readBack(t, bgpv1.Neighbor{
			Config:          bgpv1.NeighborConfig{NeighborAddress: "10.90.0.14", PeerAsn: 64513},
			GracefulRestart: &bgpv1.GracefulRestart{Enabled: true, RestartTime: 90, StaleRoutesTime: 450},
		})
		require.Equal(t, uint32(450), current.GetGracefulRestart().GetStaleRoutesTime())
		if !gracefulRestartEqual(desired.GetGracefulRestart(), current.GetGracefulRestart()) {
			t.Errorf("gracefulRestartEqual is FALSE: sent staleRoutesTime %d, got %d",
				desired.GetGracefulRestart().GetStaleRoutesTime(),
				current.GetGracefulRestart().GetStaleRoutesTime())
		}
	})

	t.Run("as-path options are claimed as one block", func(t *testing.T) {
		// Stating one of the three claims all three: the other two stop
		// inheriting and fall back to their defaults. Surprising, documented on
		// NeighborConfig, and asserted here so it cannot change silently.
		_, current := readBack(t, bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.3", PeerAsn: 64513, PeerGroup: pg,
			AllowOwnAsn: ptr(uint32(5)),
		}})
		cc := current.GetConf()
		require.Equal(t, uint32(5), cc.GetAllowOwnAsn(), "stated value must win")
		require.False(t, cc.GetReplacePeerAsn(),
			"stating allowOwnAsn claims the whole as-path block, so replacePeerAsn must NOT inherit")
		require.False(t, cc.GetAllowAspathLoopLocal(),
			"stating allowOwnAsn claims the whole as-path block, so allowAspathLoopLocal must NOT inherit")
	})

	t.Run("partial timers block claims the whole block", func(t *testing.T) {
		_, current := readBack(t, bgpv1.Neighbor{
			Config: bgpv1.NeighborConfig{NeighborAddress: "10.90.0.4", PeerAsn: 64513, PeerGroup: pg},
			Timers: &bgpv1.Timers{Config: bgpv1.TimersConfig{ConnectRetry: 77}},
		})
		got := current.GetTimers().GetConfig()
		require.Equal(t, uint64(77), got.GetConnectRetry(), "stated value must win")
		require.NotEqual(t, uint64(30), got.GetHoldTime(),
			"sending any timers block claims all of it, so the group's holdTime must NOT inherit")
	})

	t.Run("group owns peerAsn where it states one", func(t *testing.T) {
		desired, current := readBack(t, bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.5", PeerAsn: 64599, PeerGroup: pg,
		}})
		require.Equal(t, uint32(64513), current.GetConf().GetPeerAsn(),
			"peer-as is force-overwritten by the group, so the group's ASN must be in effect")
		// And the comparator must not churn on a value it cannot change.
		if !peerConfigEqual(desired, current) {
			reportPeerDiff(t, desired, current)
			t.Error("peerConfigEqual is FALSE on a group-overridden peerAsn: it would fire " +
				"UpdatePeer every reconcile for something UpdatePeer cannot fix")
		}
	})

	t.Run("owned fields survive a session rebuild", func(t *testing.T) {
		// The v1.3.4 defect: the reset path dropped field presence, so a grouped
		// member lost every field it owned - TCP-MD5 included - on the first edit
		// that rebuilt the session, and never converged. Every other case in this
		// file is a readback after a single add, which is why none of them saw it.
		n := bgpv1.Neighbor{Config: bgpv1.NeighborConfig{
			NeighborAddress: "10.90.0.6", PeerAsn: 64513, PeerGroup: pg,
			Description: ptr("owned"), AuthPassword: ptr("s3cret"), LocalAsn: 64598,
		}}
		desired := r.crdToAPINeighborWithPassword(&n, ptr("s3cret"))
		_, _ = c.DeletePeer(ctx, &gobgpapi.DeletePeerRequest{Address: n.Config.NeighborAddress})
		_, err := c.AddPeer(ctx, &gobgpapi.AddPeerRequest{Peer: desired})
		require.NoError(t, err)

		// holdTime is in NeedsResendOpenMessage as of v1.3.5, so this edit takes
		// the deleteNeighbor/addNeighbor path.
		rebuild := proto.CloneOf(desired)
		rebuild.Timers = &gobgpapi.Timers{Config: &gobgpapi.TimersConfig{HoldTime: 60, KeepaliveInterval: 20}}
		_, err = c.UpdatePeer(ctx, &gobgpapi.UpdatePeerRequest{Peer: rebuild})
		require.NoError(t, err, "UpdatePeer")

		stream, err := c.ListPeer(ctx, &gobgpapi.ListPeerRequest{Address: n.Config.NeighborAddress})
		require.NoError(t, err)
		resp, err := stream.Recv()
		require.NoError(t, err)
		cc := resp.GetPeer()

		require.Equal(t, "owned", cc.GetConf().GetDescription(), "description must survive the rebuild")
		require.Equal(t, uint32(64598), cc.GetConf().GetLocalAsn(), "localAsn must survive the rebuild")
		require.True(t, cc.GetState().GetAuthPasswordSet(), "TCP-MD5 must survive the rebuild")
	})

	t.Run("graceful restart with restartTime omitted does not churn", func(t *testing.T) {
		desired, current := readBack(t, bgpv1.Neighbor{
			Config:          bgpv1.NeighborConfig{NeighborAddress: "10.90.0.7", PeerAsn: 64513},
			GracefulRestart: &bgpv1.GracefulRestart{Enabled: true},
		})
		require.NotZero(t, current.GetGracefulRestart().GetRestartTime(),
			"gobgpd defaults restartTime from holdTime and echoes it")
		if !gracefulRestartEqual(desired.GetGracefulRestart(), current.GetGracefulRestart()) {
			t.Errorf("gracefulRestartEqual is FALSE with restartTime omitted: sent %d, got %d",
				desired.GetGracefulRestart().GetRestartTime(), current.GetGracefulRestart().GetRestartTime())
		}
	})
}
