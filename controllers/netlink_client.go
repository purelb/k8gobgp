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
	"net"

	"github.com/vishvananda/netlink"
)

// NetlinkClient abstracts netlink operations for testability.
type NetlinkClient interface {
	LinkByName(name string) (netlink.Link, error)
	AddrList(link netlink.Link, family int) ([]netlink.Addr, error)
	RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error)
}

// realNetlinkClient implements NetlinkClient using the real netlink package.
type realNetlinkClient struct{}

// NewNetlinkClient returns a NetlinkClient backed by the real kernel netlink API.
func NewNetlinkClient() NetlinkClient {
	return &realNetlinkClient{}
}

func (r *realNetlinkClient) LinkByName(name string) (netlink.Link, error) {
	return netlink.LinkByName(name)
}

func (r *realNetlinkClient) AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return netlink.AddrList(link, family)
}

func (r *realNetlinkClient) RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error) {
	return netlink.RouteListFiltered(family, filter, filterMask)
}

// interfaceOperState returns "up" or "down" for a netlink link.
// Dummy interfaces (like kube-lb0) report OperUnknown, so we treat
// both OperUp and OperUnknown as "up" when the IFF_UP flag is set.
func interfaceOperState(link netlink.Link) string {
	attrs := link.Attrs()
	switch attrs.OperState {
	case netlink.OperUp:
		return "up"
	case netlink.OperUnknown:
		// Dummy interfaces report OperUnknown; check IFF_UP flag as fallback
		if attrs.Flags&net.FlagUp != 0 {
			return "up"
		}
		return "down"
	default:
		return "down"
	}
}
