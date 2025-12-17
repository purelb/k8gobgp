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

package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBGPConfigurationSpec(t *testing.T) {
	spec := BGPConfigurationSpec{
		Global: GlobalSpec{
			ASN:      64512,
			RouterID: "192.168.1.1",
		},
		Neighbors: []Neighbor{
			{
				Config: NeighborConfig{
					NeighborAddress: "192.168.1.254",
					PeerAsn:         64513,
				},
			},
		},
	}

	assert.Equal(t, uint32(64512), spec.Global.ASN)
	assert.Equal(t, "192.168.1.1", spec.Global.RouterID)
	require.Len(t, spec.Neighbors, 1)
	assert.Equal(t, "192.168.1.254", spec.Neighbors[0].Config.NeighborAddress)
}

func TestBGPConfigurationStatus(t *testing.T) {
	now := metav1.Now()
	status := BGPConfigurationStatus{
		ObservedGeneration:   1,
		NeighborCount:        2,
		EstablishedNeighbors: 1,
		LastReconcileTime:    &now,
		Message:              "Test message",
		Conditions: []metav1.Condition{
			{
				Type:   "Ready",
				Status: metav1.ConditionTrue,
				Reason: "ReconcileSuccess",
			},
		},
	}

	assert.Equal(t, int64(1), status.ObservedGeneration)
	assert.Equal(t, 2, status.NeighborCount)
	assert.Equal(t, 1, status.EstablishedNeighbors)
	assert.NotNil(t, status.LastReconcileTime)
	assert.Equal(t, "Test message", status.Message)
	require.Len(t, status.Conditions, 1)
	assert.Equal(t, "Ready", status.Conditions[0].Type)
}

func TestGlobalSpec(t *testing.T) {
	global := GlobalSpec{
		ASN:              64512,
		RouterID:         "10.0.0.1",
		ListenPort:       179,
		ListenAddresses:  []string{"0.0.0.0", "::"},
		Families:         []string{"ipv4-unicast", "ipv6-unicast"},
		UseMultiplePaths: true,
		BindToDevice:     "eth0",
	}

	assert.Equal(t, uint32(64512), global.ASN)
	assert.Equal(t, "10.0.0.1", global.RouterID)
	assert.Equal(t, int32(179), global.ListenPort)
	assert.Len(t, global.ListenAddresses, 2)
	assert.Len(t, global.Families, 2)
	assert.True(t, global.UseMultiplePaths)
	assert.Equal(t, "eth0", global.BindToDevice)
}

func TestNeighborConfig(t *testing.T) {
	config := NeighborConfig{
		NeighborAddress:   "192.168.1.254",
		PeerAsn:           64513,
		LocalAsn:          64512,
		Description:       "Test neighbor",
		PeerGroup:         "upstream",
		AdminDown:         false,
		NeighborInterface: "eth0",
		Vrf:               "red",
	}

	assert.Equal(t, "192.168.1.254", config.NeighborAddress)
	assert.Equal(t, uint32(64513), config.PeerAsn)
	assert.Equal(t, uint32(64512), config.LocalAsn)
	assert.Equal(t, "Test neighbor", config.Description)
	assert.Equal(t, "upstream", config.PeerGroup)
	assert.False(t, config.AdminDown)
	assert.Equal(t, "eth0", config.NeighborInterface)
	assert.Equal(t, "red", config.Vrf)
}

func TestPeerGroupConfig(t *testing.T) {
	config := PeerGroupConfig{
		PeerGroupName: "upstream",
		Description:   "Upstream peers",
		PeerAsn:       64513,
		LocalAsn:      64512,
	}

	assert.Equal(t, "upstream", config.PeerGroupName)
	assert.Equal(t, "Upstream peers", config.Description)
	assert.Equal(t, uint32(64513), config.PeerAsn)
	assert.Equal(t, uint32(64512), config.LocalAsn)
}

func TestAfiSafi(t *testing.T) {
	afiSafi := AfiSafi{
		Family:  "ipv4-unicast",
		Enabled: true,
		PrefixLimit: &PrefixLimit{
			MaxPrefixes:          1000,
			ShutdownThresholdPct: 80,
		},
		AddPaths: &AddPaths{
			Receive: true,
			SendMax: 3,
		},
	}

	assert.Equal(t, "ipv4-unicast", afiSafi.Family)
	assert.True(t, afiSafi.Enabled)
	require.NotNil(t, afiSafi.PrefixLimit)
	assert.Equal(t, uint32(1000), afiSafi.PrefixLimit.MaxPrefixes)
	require.NotNil(t, afiSafi.AddPaths)
	assert.True(t, afiSafi.AddPaths.Receive)
	assert.Equal(t, uint32(3), afiSafi.AddPaths.SendMax)
}

func TestTimersConfig(t *testing.T) {
	config := TimersConfig{
		ConnectRetry:                 120,
		HoldTime:                     90,
		KeepaliveInterval:            30,
		MinimumAdvertisementInterval: 30,
	}

	assert.Equal(t, uint64(120), config.ConnectRetry)
	assert.Equal(t, uint64(90), config.HoldTime)
	assert.Equal(t, uint64(30), config.KeepaliveInterval)
	assert.Equal(t, uint64(30), config.MinimumAdvertisementInterval)
}

func TestTransport(t *testing.T) {
	transport := Transport{
		LocalAddress:  "192.168.1.1",
		PassiveMode:   true,
		BindInterface: "eth0",
	}

	assert.Equal(t, "192.168.1.1", transport.LocalAddress)
	assert.True(t, transport.PassiveMode)
	assert.Equal(t, "eth0", transport.BindInterface)
}

func TestGracefulRestart(t *testing.T) {
	gr := GracefulRestart{
		Enabled:     true,
		RestartTime: 120,
		HelperOnly:  false,
	}

	assert.True(t, gr.Enabled)
	assert.Equal(t, uint32(120), gr.RestartTime)
	assert.False(t, gr.HelperOnly)
}

func TestRouteReflector(t *testing.T) {
	rr := RouteReflector{
		RouteReflectorClient:    true,
		RouteReflectorClusterId: "1.1.1.1",
	}

	assert.True(t, rr.RouteReflectorClient)
	assert.Equal(t, "1.1.1.1", rr.RouteReflectorClusterId)
}

func TestEbgpMultihop(t *testing.T) {
	multihop := EbgpMultihop{
		Enabled:     true,
		MultihopTtl: 255,
	}

	assert.True(t, multihop.Enabled)
	assert.Equal(t, uint32(255), multihop.MultihopTtl)
}

func TestDynamicNeighbor(t *testing.T) {
	dn := DynamicNeighbor{
		Prefix:    "192.168.0.0/24",
		PeerGroup: "dynamic-peers",
	}

	assert.Equal(t, "192.168.0.0/24", dn.Prefix)
	assert.Equal(t, "dynamic-peers", dn.PeerGroup)
}

func TestVrf(t *testing.T) {
	vrf := Vrf{
		Name:     "red",
		Rd:       "65000:100",
		ImportRt: []string{"65000:100", "65000:200"},
		ExportRt: []string{"65000:100"},
	}

	assert.Equal(t, "red", vrf.Name)
	assert.Equal(t, "65000:100", vrf.Rd)
	assert.Len(t, vrf.ImportRt, 2)
	assert.Len(t, vrf.ExportRt, 1)
}

func TestNetlinkImport(t *testing.T) {
	ni := NetlinkImport{
		Enabled:       true,
		Vrf:           "red",
		InterfaceList: []string{"eth*", "ens*"},
	}

	assert.True(t, ni.Enabled)
	assert.Equal(t, "red", ni.Vrf)
	assert.Len(t, ni.InterfaceList, 2)
}

func TestNetlinkExport(t *testing.T) {
	validateNexthop := true
	ne := NetlinkExport{
		Enabled:           true,
		DampeningInterval: 100,
		RouteProtocol:     186,
		Rules: []NetlinkExportRule{
			{
				Name:            "default",
				CommunityList:   []string{"65000:100"},
				TableId:         0,
				Metric:          20,
				ValidateNexthop: &validateNexthop,
			},
		},
	}

	assert.True(t, ne.Enabled)
	assert.Equal(t, uint32(100), ne.DampeningInterval)
	assert.Equal(t, int32(186), ne.RouteProtocol)
	require.Len(t, ne.Rules, 1)
	assert.Equal(t, "default", ne.Rules[0].Name)
	assert.True(t, *ne.Rules[0].ValidateNexthop)
}

func TestPolicyDefinition(t *testing.T) {
	policy := PolicyDefinition{
		Name: "accept-all",
		Statements: []Statement{
			{
				Name: "stmt1",
				Conditions: Conditions{
					PrefixSet: &MatchSet{
						Name:  "allowed-prefixes",
						Match: "any",
					},
				},
				Actions: Actions{
					RouteAction: "accept",
					LocalPref:   100,
				},
			},
		},
	}

	assert.Equal(t, "accept-all", policy.Name)
	require.Len(t, policy.Statements, 1)
	assert.Equal(t, "stmt1", policy.Statements[0].Name)
	assert.Equal(t, "accept", policy.Statements[0].Actions.RouteAction)
}

func TestDefinedSet(t *testing.T) {
	ds := DefinedSet{
		Type: "prefix",
		Name: "allowed-prefixes",
		Prefixes: []Prefix{
			{
				IpPrefix:      "10.0.0.0/8",
				MaskLengthMin: 8,
				MaskLengthMax: 24,
			},
		},
	}

	assert.Equal(t, "prefix", ds.Type)
	assert.Equal(t, "allowed-prefixes", ds.Name)
	require.Len(t, ds.Prefixes, 1)
	assert.Equal(t, "10.0.0.0/8", ds.Prefixes[0].IpPrefix)
}

func TestApplyPolicy(t *testing.T) {
	ap := ApplyPolicy{
		ImportPolicy: &PolicyAssignment{
			Name:          "import",
			Policies:      []string{"filter-bogons"},
			DefaultAction: "reject",
		},
		ExportPolicy: &PolicyAssignment{
			Name:          "export",
			Policies:      []string{"announce-local"},
			DefaultAction: "accept",
		},
	}

	require.NotNil(t, ap.ImportPolicy)
	assert.Equal(t, "import", ap.ImportPolicy.Name)
	assert.Equal(t, "reject", ap.ImportPolicy.DefaultAction)
	require.NotNil(t, ap.ExportPolicy)
	assert.Equal(t, "export", ap.ExportPolicy.Name)
	assert.Equal(t, "accept", ap.ExportPolicy.DefaultAction)
}

func TestCommunityAction(t *testing.T) {
	ca := CommunityAction{
		Type:        "add",
		Communities: []string{"65000:100", "65000:200"},
	}

	assert.Equal(t, "add", ca.Type)
	assert.Len(t, ca.Communities, 2)
}

func TestMedAction(t *testing.T) {
	ma := MedAction{
		Type:  "replace",
		Value: 100,
	}

	assert.Equal(t, "replace", ma.Type)
	assert.Equal(t, int64(100), ma.Value)
}

func TestAsPrependAction(t *testing.T) {
	apa := AsPrependAction{
		Asn:    64512,
		Repeat: 3,
	}

	assert.Equal(t, uint32(64512), apa.Asn)
	assert.Equal(t, uint32(3), apa.Repeat)
}

func TestRouteSelectionOptions(t *testing.T) {
	rso := RouteSelectionOptions{
		AlwaysCompareMed:        true,
		AdvertiseInactiveRoutes: false,
	}

	assert.True(t, rso.AlwaysCompareMed)
	assert.False(t, rso.AdvertiseInactiveRoutes)
}

func TestDefaultRouteDistance(t *testing.T) {
	drd := DefaultRouteDistance{
		ExternalRouteDistance: 20,
		InternalRouteDistance: 200,
	}

	assert.Equal(t, uint32(20), drd.ExternalRouteDistance)
	assert.Equal(t, uint32(200), drd.InternalRouteDistance)
}

func TestConfederation(t *testing.T) {
	conf := Confederation{
		Enabled:      true,
		Identifier:   64512,
		MemberAsList: []uint32{64513, 64514},
	}

	assert.True(t, conf.Enabled)
	assert.Equal(t, uint32(64512), conf.Identifier)
	assert.Len(t, conf.MemberAsList, 2)
}
