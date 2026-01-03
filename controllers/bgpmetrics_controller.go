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
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GoBGPStatsClient interface for testing - allows mocking gRPC calls
type GoBGPStatsClient interface {
	ListPeer(ctx context.Context, req *gobgpapi.ListPeerRequest, opts ...grpc.CallOption) (gobgpapi.GoBgpService_ListPeerClient, error)
	GetTable(ctx context.Context, req *gobgpapi.GetTableRequest, opts ...grpc.CallOption) (*gobgpapi.GetTableResponse, error)
	GetBgp(ctx context.Context, req *gobgpapi.GetBgpRequest, opts ...grpc.CallOption) (*gobgpapi.GetBgpResponse, error)
}

// MetricsConfig holds configuration for metrics collection
type MetricsConfig struct {
	PollInterval             time.Duration
	EnablePerNeighborMetrics bool
	MaxNeighborsForMetrics   int
}

// BGPMetricsController collects BGP metrics from gobgpd
type BGPMetricsController struct {
	Log           logr.Logger
	GoBGPEndpoint string
	Config        MetricsConfig

	// For testing - if nil, creates real client
	ClientFactory func(conn *grpc.ClientConn) GoBGPStatsClient

	// Internal state
	collectMu           sync.Mutex
	consecutiveFailures int
	currentInterval     time.Duration
}

// Start implements controller-runtime Runnable interface
func (m *BGPMetricsController) Start(ctx context.Context) error {
	log := m.Log.WithName("metrics-collector")

	interval := m.Config.PollInterval
	if interval == 0 {
		interval = 15 * time.Second // Default 15s
	}
	m.currentInterval = interval

	// Validate minimum interval
	if interval < 15*time.Second {
		return fmt.Errorf("metrics-poll-interval must be >= 15s, got %v", interval)
	}

	log.Info("Starting BGP metrics collector", "interval", interval,
		"perNeighborMetrics", m.Config.EnablePerNeighborMetrics)

	// Wait for gobgpd to be ready before first collection
	time.Sleep(5 * time.Second)

	// Initial collection with retries
	for i := 0; i < 3; i++ {
		if ctx.Err() != nil {
			return nil
		}
		if err := m.collectMetricsWithTimeout(ctx); err == nil {
			break
		}
		time.Sleep(time.Duration(1<<uint(i)) * time.Second)
	}

	ticker := time.NewTicker(m.currentInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("Stopping metrics collector")
			return nil
		case <-ticker.C:
			if err := m.collectMetricsWithTimeout(ctx); err != nil {
				m.consecutiveFailures++
				if m.consecutiveFailures >= 5 {
					// Exponential backoff: double interval up to 5 minutes
					newInterval := m.currentInterval * 2
					if newInterval > 5*time.Minute {
						newInterval = 5 * time.Minute
					}
					if newInterval != m.currentInterval {
						log.Info("Backing off metrics collection due to repeated failures",
							"newInterval", newInterval, "consecutiveFailures", m.consecutiveFailures)
						ticker.Reset(newInterval)
						m.currentInterval = newInterval
					}
				}
			} else {
				// Reset on success
				if m.consecutiveFailures >= 5 {
					log.Info("Metrics collection recovered, resetting interval", "interval", interval)
					ticker.Reset(interval)
					m.currentInterval = interval
				}
				m.consecutiveFailures = 0
			}
		}
	}
}

func (m *BGPMetricsController) collectMetricsWithTimeout(parentCtx context.Context) error {
	// Prevent concurrent collection
	if !m.collectMu.TryLock() {
		m.Log.V(1).Info("Skipping metrics collection - previous cycle still running")
		metricsCollectionSkipped.Inc()
		return nil
	}
	defer m.collectMu.Unlock()

	// Create timeout context - 10 seconds max for entire collection
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()

	start := time.Now()
	err := m.collectMetrics(ctx)
	duration := time.Since(start).Seconds()

	metricsCollectionDuration.Observe(duration)
	if duration > 5 {
		m.Log.Info("Slow metrics collection", "duration_seconds", duration)
	}

	return err
}

func (m *BGPMetricsController) collectMetrics(ctx context.Context) error {
	log := m.Log.WithName("collect")

	// Connect to gobgpd
	endpoint := m.GoBGPEndpoint
	if endpoint == "" {
		endpoint = "unix:///var/run/gobgp/gobgp.sock"
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.V(1).Info("Failed to connect to gobgpd for metrics", "error", err)
		metricsCollectionErrors.Inc()
		RecordGoBGPConnection(endpoint, false)
		return err
	}
	defer func() { _ = conn.Close() }()

	RecordGoBGPConnection(endpoint, true)

	// Get client (use factory for testing, or create real one)
	var client GoBGPStatsClient
	if m.ClientFactory != nil {
		client = m.ClientFactory(conn)
	} else {
		client = gobgpapi.NewGoBgpServiceClient(conn)
	}

	// Collect all metrics - continue on partial failure
	var errs []error

	if err := m.collectNeighborMetrics(ctx, client); err != nil {
		log.V(1).Info("Failed to collect neighbor metrics", "error", err)
		errs = append(errs, err)
	}

	if err := m.collectRibMetrics(ctx, client); err != nil {
		log.V(1).Info("Failed to collect RIB metrics", "error", err)
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		metricsCollectionErrors.Inc()
		return fmt.Errorf("partial collection failure: %d errors", len(errs))
	}
	return nil
}

func (m *BGPMetricsController) collectNeighborMetrics(ctx context.Context, client GoBGPStatsClient) error {
	stream, err := client.ListPeer(ctx, &gobgpapi.ListPeerRequest{
		EnableAdvertised: true,
	})
	if err != nil {
		return fmt.Errorf("ListPeer failed: %w", err)
	}

	var total, established, active, idle int
	var totalReceived, totalAccepted, totalAdvertised uint64
	const maxNeighbors = 5000 // Safety limit

	// Reset per-neighbor metrics before collecting to handle removed neighbors
	if m.Config.EnablePerNeighborMetrics {
		bgpNeighborRoutesReceived.Reset()
		bgpNeighborRoutesAccepted.Reset()
		bgpNeighborRoutesAdvertised.Reset()
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("ListPeer stream error: %w", err)
		}

		peer := resp.Peer
		total++

		if total > maxNeighbors {
			m.Log.Error(fmt.Errorf("too many neighbors"),
				"Aborting metrics collection - neighbor count exceeds limit",
				"limit", maxNeighbors)
			return fmt.Errorf("neighbor count %d exceeds limit %d", total, maxNeighbors)
		}

		// Count by session state
		switch peer.State.SessionState {
		case gobgpapi.PeerState_SESSION_STATE_ESTABLISHED:
			established++
		case gobgpapi.PeerState_SESSION_STATE_ACTIVE:
			active++
		case gobgpapi.PeerState_SESSION_STATE_IDLE:
			idle++
		}

		// Per-neighbor AFI stats (only if enabled and under limit)
		if m.Config.EnablePerNeighborMetrics {
			maxForMetrics := m.Config.MaxNeighborsForMetrics
			if maxForMetrics == 0 {
				maxForMetrics = 200 // Default limit
			}

			if total <= maxForMetrics {
				neighborAddr := sanitizeNeighborAddress(peer.State.NeighborAddress)

				for _, afiSafi := range peer.AfiSafis {
					family := familyToString(afiSafi.State.Family)

					bgpNeighborRoutesReceived.WithLabelValues(neighborAddr, family).
						Set(float64(afiSafi.State.Received))
					bgpNeighborRoutesAccepted.WithLabelValues(neighborAddr, family).
						Set(float64(afiSafi.State.Accepted))
					bgpNeighborRoutesAdvertised.WithLabelValues(neighborAddr, family).
						Set(float64(afiSafi.State.Advertised))

					totalReceived += afiSafi.State.Received
					totalAccepted += afiSafi.State.Accepted
					totalAdvertised += afiSafi.State.Advertised
				}
			} else if total == maxForMetrics+1 {
				m.Log.Info("Per-neighbor metrics limit reached, exporting aggregates only",
					"neighbors", total, "limit", maxForMetrics)
				metricsCardinalityLimitHit.Inc()
			}
		} else {
			// Still collect aggregate stats even if per-neighbor is disabled
			for _, afiSafi := range peer.AfiSafis {
				totalReceived += afiSafi.State.Received
				totalAccepted += afiSafi.State.Accepted
				totalAdvertised += afiSafi.State.Advertised
			}
		}
	}

	// Update global neighbor counts
	bgpNeighborsTotal.Set(float64(total))
	bgpNeighborsEstablishedTotal.Set(float64(established))
	bgpNeighborsActive.Set(float64(active))
	bgpNeighborsIdle.Set(float64(idle))

	// Update aggregate route counts
	bgpRoutesReceivedTotal.Set(float64(totalReceived))
	bgpRoutesAcceptedTotal.Set(float64(totalAccepted))
	bgpRoutesAdvertisedTotal.Set(float64(totalAdvertised))

	return nil
}

func (m *BGPMetricsController) collectRibMetrics(ctx context.Context, client GoBGPStatsClient) error {
	// Get configured families dynamically
	families, err := m.getConfiguredFamilies(ctx, client)
	if err != nil {
		m.Log.V(1).Info("Failed to get configured families, using defaults", "error", err)
		families = defaultFamilies()
	}

	// Reset before repopulating to handle removed families
	bgpRibRoutes.Reset()

	for _, family := range families {
		resp, err := client.GetTable(ctx, &gobgpapi.GetTableRequest{
			TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL,
			Family:    family,
		})
		if err != nil {
			continue // Skip this family
		}

		familyStr := familyToString(family)
		bgpRibRoutes.WithLabelValues(familyStr).Set(float64(resp.NumPath))
	}

	return nil
}

func (m *BGPMetricsController) getConfiguredFamilies(ctx context.Context, client GoBGPStatsClient) ([]*gobgpapi.Family, error) {
	resp, err := client.GetBgp(ctx, &gobgpapi.GetBgpRequest{})
	if err != nil {
		return nil, err
	}

	if resp.Global == nil || len(resp.Global.Families) == 0 {
		return defaultFamilies(), nil
	}

	// Convert uint32 encoded families (AFI << 16 | SAFI) to Family structs
	families := make([]*gobgpapi.Family, 0, len(resp.Global.Families))
	for _, encoded := range resp.Global.Families {
		// #nosec G115 -- AFI/SAFI values are defined by BGP RFC and fit in int32
		afi := gobgpapi.Family_Afi(encoded >> 16)
		// #nosec G115 -- AFI/SAFI values are defined by BGP RFC and fit in int32
		safi := gobgpapi.Family_Safi(encoded & 0xFFFF)
		families = append(families, &gobgpapi.Family{
			Afi:  afi,
			Safi: safi,
		})
	}
	return families, nil
}

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

// sanitizeNeighborAddress validates and sanitizes neighbor address for use in labels
func sanitizeNeighborAddress(addr string) string {
	// Validate IP address format
	if ip := net.ParseIP(addr); ip != nil {
		return ip.String() // Use canonical form
	}
	// Invalid IP - return safe fallback
	return "invalid"
}
