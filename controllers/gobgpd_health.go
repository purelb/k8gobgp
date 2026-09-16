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
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	gobgpapi "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// gobgpdProbeTimeout bounds a single readiness check. GetBgp reads configuration
// rather than the RIB, so it is O(1) and this is generous.
const gobgpdProbeTimeout = 3 * time.Second

// GoBGPDChecker answers "is gobgpd up and serving?" for the readiness probe.
//
// This is the only thing that verifies gobgpd at all. The manager and the daemon
// are separate processes in one container, and before this a wedged or dead
// gobgpd left the pod reporting Ready: both probes were healthz.Ping plus a
// check that a BGPNodeStatus had been written recently.
//
// Deliberately not in the liveness probe. If gobgpd dies outright, entrypoint.sh
// notices and the container restarts, so that case is self-healing; readiness
// covers the two the supervisor does not - the window before the restart lands,
// and gobgpd alive but not serving.
//
// Equally deliberately, this does not look at BGP session state. Zero peers is a
// valid steady state - a deployment using local-address mode runs gobgpd with no
// peering at all - so a probe that failed on sessions would mark those nodes
// NotReady forever. Session health belongs in alerts, not in scheduling.
type GoBGPDChecker struct {
	// Endpoint is the gRPC endpoint, e.g. unix:///var/run/gobgp/gobgp.sock.
	Endpoint string

	mu     sync.Mutex
	conn   *grpc.ClientConn
	client gobgpapi.GoBgpServiceClient
}

// NewGoBGPDChecker returns a checker for the given endpoint.
func NewGoBGPDChecker(endpoint string) *GoBGPDChecker {
	if endpoint == "" {
		endpoint = defaultGoBGPEndpoint
	}
	return &GoBGPDChecker{Endpoint: endpoint}
}

// clientLocked returns a client, dialing once and reusing the connection.
//
// grpc.NewClient is lazy: it does not connect, and it fails only on a malformed
// target. That is precisely why this type exists rather than the gauge being set
// from a dial - see Check.
func (c *GoBGPDChecker) clientLocked() (gobgpapi.GoBgpServiceClient, error) {
	if c.client != nil {
		return c.client, nil
	}

	target := c.Endpoint
	if strings.HasPrefix(target, "unix://") {
		target = "unix:" + strings.TrimPrefix(target, "unix://")
	}

	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("creating gobgpd client for %q: %w", c.Endpoint, err)
	}
	c.conn = conn
	c.client = gobgpapi.NewGoBgpServiceClient(conn)
	return c.client, nil
}

// Check is the readyz check. It issues one GetBgp and reports the outcome.
//
// It also owns k8gobgp_gobgpd_connection_status. That metric was previously set
// from the result of grpc.NewClient, which never dials - so it read 1 whether or
// not gobgpd was alive, and the severity-critical K8GoBGPDaemonDisconnected
// alert built on it had never once fired. Driving both the probe and the gauge
// from the same RPC gives one answer instead of two that could disagree, and at
// the probe's cadence rather than the poll loop's.
func (c *GoBGPDChecker) Check(req *http.Request) error {
	ctx := context.Background()
	if req != nil {
		ctx = req.Context()
	}
	ctx, cancel := context.WithTimeout(ctx, gobgpdProbeTimeout)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	client, err := c.clientLocked()
	if err != nil {
		RecordGoBGPConnection(c.Endpoint, false)
		return err
	}

	if _, err := client.GetBgp(ctx, &gobgpapi.GetBgpRequest{}); err != nil {
		RecordGoBGPConnection(c.Endpoint, false)
		RecordGoBGPConnectionError(c.Endpoint)
		return fmt.Errorf("gobgpd not serving at %q: %w", c.Endpoint, err)
	}

	RecordGoBGPConnection(c.Endpoint, true)
	return nil
}

// Close releases the connection. Only used in tests; the process holds this for
// its lifetime otherwise.
func (c *GoBGPDChecker) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		err := c.conn.Close()
		c.conn, c.client = nil, nil
		return err
	}
	return nil
}
