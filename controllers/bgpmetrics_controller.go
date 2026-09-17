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
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GoBGPStatsClient is the RPC surface this poll loop uses. Kept as an interface
// so tests can supply what gobgpd actually returns.
//
// ListPeer is deliberately absent: the per-neighbor series it fed are emitted
// natively by gobgp-netlink now. GoBGPNodeStatusClient, which still needs it for
// the CR status, declares it itself.
type GoBGPStatsClient interface {
	GetTable(ctx context.Context, req *gobgpapi.GetTableRequest, opts ...grpc.CallOption) (*gobgpapi.GetTableResponse, error)
	GetBgp(ctx context.Context, req *gobgpapi.GetBgpRequest, opts ...grpc.CallOption) (*gobgpapi.GetBgpResponse, error)
}

// MetricsConfig holds configuration for metrics collection.
//
// EnablePerNeighborMetrics and MaxNeighborsForMetrics are gone with the
// per-neighbor metrics they governed. gobgp-netlink emits per-peer series
// itself, uncapped - bound them at scrape time; docs/metrics.md carries a
// keep-list.
type MetricsConfig struct {
	PollInterval time.Duration
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

	log.Info("Starting BGP metrics collector", "interval", interval)

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

// sanitizeNeighborKey validates a neighborKey() result for use as a label.
//
// Uses netip rather than net.ParseIP because unnumbered peers carry a *zoned*
// link-local address (fe80::1%eth0), which net.ParseIP rejects — it would
// return the literal "invalid" for every unnumbered peer, collapsing them all
// onto one series so their states overwrite each other. Zones are stripped:
// the interface is already carried by the "iface:" form where it matters.
