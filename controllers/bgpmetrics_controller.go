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
	"fmt"
	"strings"

	gobgpapi "github.com/osrg/gobgp/v4/api"
)

func defaultFamilies() []*gobgpapi.Family {
	return []*gobgpapi.Family{
		{Afi: gobgpapi.Family_AFI_IP, Safi: gobgpapi.Family_SAFI_UNICAST},
		{Afi: gobgpapi.Family_AFI_IP6, Safi: gobgpapi.Family_SAFI_UNICAST},
	}
}

// familyToString converts gRPC Family to human-readable string
// Uses underscores for Prometheus compatibility
func familyToString(f *gobgpapi.Family) string {
	if f == nil {
		return "unknown"
	}

	// Map AFI to human-readable string
	var afiStr string
	switch f.Afi {
	case gobgpapi.Family_AFI_IP:
		afiStr = "ipv4"
	case gobgpapi.Family_AFI_IP6:
		afiStr = "ipv6"
	case gobgpapi.Family_AFI_L2VPN:
		afiStr = "l2vpn"
	default:
		afiStr = strings.ToLower(strings.TrimPrefix(f.Afi.String(), "AFI_"))
	}

	// Map SAFI to human-readable string
	var safiStr string
	switch f.Safi {
	case gobgpapi.Family_SAFI_UNICAST:
		safiStr = "unicast"
	case gobgpapi.Family_SAFI_MULTICAST:
		safiStr = "multicast"
	case gobgpapi.Family_SAFI_EVPN:
		safiStr = "evpn"
	case gobgpapi.Family_SAFI_FLOW_SPEC_UNICAST:
		safiStr = "flowspec_unicast"
	default:
		safiStr = strings.ToLower(strings.TrimPrefix(f.Safi.String(), "SAFI_"))
	}

	return fmt.Sprintf("%s_%s", afiStr, safiStr)
}

// sanitizeNeighborKey validates a neighborKey() result for use as a label.
//
// Uses netip rather than net.ParseIP because unnumbered peers carry a *zoned*
// link-local address (fe80::1%eth0), which net.ParseIP rejects — it would
// return the literal "invalid" for every unnumbered peer, collapsing them all
// onto one series so their states overwrite each other. Zones are stripped:
// the interface is already carried by the "iface:" form where it matters.
