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
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	gobgpapi "github.com/osrg/gobgp/v4/api"
	"github.com/vishvananda/netlink"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bgpv1 "github.com/purelb/k8gobgp/api/v1"
)

// +kubebuilder:rbac:groups=bgp.purelb.io,resources=bgpnodestatuses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=bgp.purelb.io,resources=bgpnodestatuses/status,verbs=get;update;patch

// Array size caps to prevent etcd size issues with large RIBs.
//
// etcd refuses a value over 1.5 MiB and the apiserver rejects earlier still, so
// every unbounded list here needs a bound. TestStatusCapsFitEtcdBudget measures
// the worst case these caps permit and is the reason for the specific numbers -
// re-run it rather than adjusting a cap by eye.
const (
	maxLocalRoutes       = 500
	maxReceivedRoutes    = 100
	maxExportedRoutes    = 500
	maxImportedAddresses = 500
	maxNeighbors         = 128
	maxVRFs              = 64
	// Per local route. This is the dominant term in the whole object: it is
	// built one address per established neighbor per local route, so it
	// multiplies rather than adds. 16 is enough to name the peers a route is
	// actually reaching; beyond that the count is the useful signal.
	maxAdvertisedTo = 16
)

// GoBGPNodeStatusClient extends GoBGPStatsClient with additional methods needed by the reporter.
type GoBGPNodeStatusClient interface {
	GoBGPStatsClient
	// Declared here rather than inherited: the metrics poll loop no longer calls
	// ListPeer, but the CR status still reports per-neighbor session state.
	ListPeer(ctx context.Context, in *gobgpapi.ListPeerRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[gobgpapi.ListPeerResponse], error)
	ListPath(ctx context.Context, in *gobgpapi.ListPathRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[gobgpapi.ListPathResponse], error)
	GetNetlink(ctx context.Context, in *gobgpapi.GetNetlinkRequest, opts ...grpc.CallOption) (*gobgpapi.GetNetlinkResponse, error)
	GetBfdServerState(ctx context.Context, in *gobgpapi.GetBfdServerStateRequest, opts ...grpc.CallOption) (*gobgpapi.GetBfdServerStateResponse, error)
	ListNetlinkExportRules(ctx context.Context, in *gobgpapi.ListNetlinkExportRulesRequest, opts ...grpc.CallOption) (*gobgpapi.ListNetlinkExportRulesResponse, error)
	ListNetlinkExport(ctx context.Context, in *gobgpapi.ListNetlinkExportRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[gobgpapi.ListNetlinkExportResponse], error)
	ListVrf(ctx context.Context, in *gobgpapi.ListVrfRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[gobgpapi.ListVrfResponse], error)
}

// BGPNodeStatusReporter collects BGP state and writes it to a BGPNodeStatus CRD.
// Implements manager.Runnable.
type BGPNodeStatusReporter struct {
	Client client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	GoBGPEndpoint string
	NodeName      string

	// Dependency injection for testing
	GoBGPClientFactory   func(conn *grpc.ClientConn) GoBGPNodeStatusClient
	NetlinkClientFactory func() NetlinkClient

	// Config (updated by reconciler via atomic ops)
	enabled          atomic.Bool
	heartbeatSeconds atomic.Int32
	routerID         atomic.Pointer[string]
	routerIDSource   atomic.Pointer[string]
	asn              atomic.Uint32

	// Internal state (single-goroutine access in Start loop, no lock needed)
	lastWrittenStatus   *bgpv1.BGPNodeStatusData
	lastWriteTime       time.Time
	consecutiveFailures int
}

// Reporter staleness is no longer a readiness check. It used to be registered as
// one, which meant a slow apiserver took the pod out of service while its BGP was
// healthy - conflating "this reporter is behind" with "this pod cannot do its
// job". The signal still exists, as
// k8gobgp_nodestatus_last_successful_write_timestamp_seconds; alert on it with
// `time() - <metric> > 300`. Readiness now reports only whether the manager and
// gobgpd are both running - see GoBGPDChecker.

// UpdateConfig is called by the reconciler to pass configuration to the reporter.
func (r *BGPNodeStatusReporter) UpdateConfig(enabled bool, heartbeat int32, routerID, routerIDSource string, asn uint32) {
	r.enabled.Store(enabled)
	r.heartbeatSeconds.Store(heartbeat)
	r.routerID.Store(&routerID)
	r.routerIDSource.Store(&routerIDSource)
	r.asn.Store(asn)
}

// Start implements manager.Runnable. It runs in its own goroutine.
func (r *BGPNodeStatusReporter) Start(ctx context.Context) error {
	log := r.Log.WithName("reporter")

	// Set defaults before first reconcile passes config
	r.enabled.Store(true)
	r.heartbeatSeconds.Store(60)

	log.Info("Starting BGPNodeStatus reporter", "node", r.NodeName)

	// Wait for GoBGP to be ready
	time.Sleep(5 * time.Second)

	// Ensure the BGPNodeStatus object exists
	for i := 0; i < 5; i++ {
		if ctx.Err() != nil {
			return nil
		}
		if err := r.ensureBGPNodeStatus(ctx); err != nil {
			log.V(1).Info("Failed to ensure BGPNodeStatus, retrying", "error", err, "attempt", i+1)
			time.Sleep(time.Duration(1<<uint(i)) * time.Second)
			continue
		}
		break
	}

	heartbeat := time.Duration(r.heartbeatSeconds.Load()) * time.Second
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	// Initial collection
	r.collectAndWrite(ctx, log)

	for {
		select {
		case <-ctx.Done():
			log.Info("Shutting down BGPNodeStatus reporter")
			r.markShutdown(log)
			return nil
		case <-ticker.C:
			if !r.enabled.Load() {
				log.V(1).Info("BGPNodeStatus reporting disabled, skipping collection")
				nodeStatusWriteTotal.WithLabelValues("skipped").Inc()
				continue
			}

			r.collectAndWrite(ctx, log)

			// Update ticker if heartbeat changed
			newHeartbeat := time.Duration(r.heartbeatSeconds.Load()) * time.Second
			if newHeartbeat != heartbeat {
				heartbeat = newHeartbeat
				ticker.Reset(heartbeat)
				log.Info("Heartbeat interval changed", "interval", heartbeat)
			}
		}
	}
}

func (r *BGPNodeStatusReporter) collectAndWrite(ctx context.Context, log logr.Logger) {
	collectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	start := time.Now()
	status, err := r.collectStatus(collectCtx, log)
	duration := time.Since(start).Seconds()
	nodeStatusCollectionDuration.Observe(duration)

	if err != nil {
		r.consecutiveFailures++
		log.V(1).Info("Failed to collect status", "error", err, "consecutiveFailures", r.consecutiveFailures)
		nodeStatusWriteTotal.WithLabelValues("error").Inc()
		return
	}

	if err := r.writeStatus(ctx, status, log); err != nil {
		r.consecutiveFailures++
		log.V(1).Info("Failed to write status", "error", err, "consecutiveFailures", r.consecutiveFailures)
		return
	}

	r.consecutiveFailures = 0
}

func (r *BGPNodeStatusReporter) ensureBGPNodeStatus(ctx context.Context) error {
	existing := &bgpv1.BGPNodeStatus{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: r.NodeName}, existing)
	if err == nil {
		// Already exists (pod restart). Verify OwnerReference is present.
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	// Create new — get Node for OwnerReference UID
	node := &corev1.Node{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: r.NodeName}, node); err != nil {
		return fmt.Errorf("get node for OwnerReference: %w", err)
	}

	obj := &bgpv1.BGPNodeStatus{
		ObjectMeta: metav1.ObjectMeta{
			Name: r.NodeName,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Node",
				Name:       node.Name,
				UID:        node.UID,
			}},
		},
	}
	return r.Client.Create(ctx, obj)
}

func (r *BGPNodeStatusReporter) markShutdown(log logr.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	existing := &bgpv1.BGPNodeStatus{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: r.NodeName}, existing); err != nil {
		log.V(1).Info("Could not get BGPNodeStatus for shutdown marker", "error", err)
		return
	}

	existing.Status.Healthy = false
	now := metav1.Now()
	existing.Status.LastUpdated = &now
	setCondition(&existing.Status, "Ready", metav1.ConditionFalse, "AgentShutdown", "k8gobgp agent is shutting down")

	if err := r.Client.Status().Update(ctx, existing); err != nil {
		log.V(1).Info("Could not set shutdown condition on BGPNodeStatus", "error", err)
	}
}

// collectStatus gathers the full status snapshot from GoBGP gRPC + netlink.
func (r *BGPNodeStatusReporter) collectStatus(ctx context.Context, log logr.Logger) (*bgpv1.BGPNodeStatusData, error) {
	endpoint := r.GoBGPEndpoint
	if endpoint == "" {
		endpoint = "unix:///var/run/gobgp/gobgp.sock"
	}

	conn, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to gobgpd: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var apiClient GoBGPNodeStatusClient
	if r.GoBGPClientFactory != nil {
		apiClient = r.GoBGPClientFactory(conn)
	} else {
		apiClient = gobgpapi.NewGoBgpServiceClient(conn)
	}

	nlClient := NewNetlinkClient()
	if r.NetlinkClientFactory != nil {
		nlClient = r.NetlinkClientFactory()
	}

	status := &bgpv1.BGPNodeStatusData{
		NodeName: r.NodeName,
	}

	// Populate identity from atomic config
	if rid := r.routerID.Load(); rid != nil {
		status.RouterID = *rid
	}
	if src := r.routerIDSource.Load(); src != nil {
		status.RouterIDSource = *src
	}
	status.ASN = r.asn.Load()

	// Collect neighbors
	neighbors, err := r.collectNeighborStatus(ctx, apiClient)
	if err != nil {
		log.V(1).Info("Failed to collect neighbor status", "error", err)
	} else {
		// NeighborCount is set from the full list, before the cap, so it stays
		// the honest answer to "how many peers does this node have".
		status.NeighborCount = len(neighbors)
		if len(neighbors) > maxNeighbors {
			neighbors = neighbors[:maxNeighbors]
			status.Truncated = true
		}
		status.Neighbors = neighbors
	}

	// Collect netlink import status
	importStatus, err := r.collectNetlinkImportStatus(ctx, apiClient, nlClient, log)
	if err != nil {
		log.V(1).Info("Failed to collect netlink import status", "error", err)
	} else {
		status.NetlinkImport = importStatus
	}

	// Collect RIB status (pass neighbors so adj-out can be queried for advertisedTo)
	ribStatus, err := r.collectRIBStatus(ctx, apiClient, status.Neighbors)
	if err != nil {
		log.V(1).Info("Failed to collect RIB status", "error", err)
	} else {
		status.RIB = ribStatus
	}

	// Collect netlink export status
	exportStatus, err := r.collectNetlinkExportStatus(ctx, apiClient, nlClient, log)
	if err != nil {
		log.V(1).Info("Failed to collect netlink export status", "error", err)
	} else {
		status.NetlinkExport = exportStatus
	}

	// Collect VRF status
	vrfs, err := r.collectVRFStatus(ctx, apiClient)
	if err != nil {
		log.V(1).Info("Failed to collect VRF status", "error", err)
	} else {
		if len(vrfs) > maxVRFs {
			vrfs = vrfs[:maxVRFs]
			status.Truncated = true
		}
		status.VRFs = vrfs
	}

	// BFD server state.
	//
	// Measured against gobgpd v1.3.0: a node with no BFD peers returns a
	// non-nil State with listening=false, not the nil State that would mean
	// "never started". The nil case is still handled, because the two are
	// reported differently - nil is omitted from the CR entirely - but on this
	// version the normal non-BFD node reports listening: false.
	//
	// Which is exactly why deriveHealth must not consume this unguarded: every
	// node in a cluster that does not use BFD reports false here. See
	// bfdDegraded.
	if bfdResp, err := apiClient.GetBfdServerState(ctx, &gobgpapi.GetBfdServerStateRequest{}); err != nil {
		log.V(1).Info("Failed to get BFD server state", "error", err)
	} else if bfdResp.GetState() != nil {
		status.BFDServer = &bgpv1.BFDServerStatus{Listening: bfdResp.GetState().GetListening()}
	}

	// Derive health
	status.Healthy = r.deriveHealth(status)
	r.setReadyCondition(status)

	return status, nil
}

func (r *BGPNodeStatusReporter) collectNeighborStatus(ctx context.Context, client GoBGPNodeStatusClient) ([]bgpv1.NeighborStatus, error) {
	stream, err := client.ListPeer(ctx, &gobgpapi.ListPeerRequest{
		EnableAdvertised: true,
	})
	if err != nil {
		return nil, fmt.Errorf("ListPeer: %w", err)
	}

	var neighbors []bgpv1.NeighborStatus
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return neighbors, fmt.Errorf("ListPeer stream: %w", err)
		}

		peer := resp.Peer
		if peer == nil || peer.State == nil {
			continue
		}

		ns := bgpv1.NeighborStatus{
			Address:  peer.State.NeighborAddress,
			State:    peerStateToString(peer.State.SessionState),
			LocalASN: peer.Conf.GetLocalAsn(),
			PeerASN:  peer.Conf.GetPeerAsn(),
		}

		if peer.Conf != nil {
			ns.Description = peer.Conf.Description
		}

		// Gated on BFD actually being enabled. NewPeerFromConfigStruct builds
		// State.BfdState for every peer, so without this gate every neighbor
		// gains a bfd block reporting Unspecified - noise in every CR on every
		// node, and bytes in the one list that has no cap.
		if peer.GetBfd().GetEnabled() {
			bs := peer.GetState().GetBfdState()
			ns.BFD = &bgpv1.BFDStatus{
				SessionState:         bfdStateToString(bs.GetSessionState()),
				RemoteSessionState:   bfdStateToString(bs.GetRemoteSessionState()),
				LocalDiagnosticCode:  bfdDiagToString(bs.GetLocalDiagnosticCode()),
				RemoteDiagnosticCode: bfdDiagToString(bs.GetRemoteDiagnosticCode()),
				FailureTransitions:   bs.GetFailureTransitions(),
			}
		}

		// Session uptime (Established) or last error (non-Established)
		if peer.State.SessionState == gobgpapi.PeerState_SESSION_STATE_ESTABLISHED {
			if peer.Timers != nil && peer.Timers.State != nil && peer.Timers.State.Uptime != nil {
				uptime := peer.Timers.State.Uptime.AsTime()
				t := metav1.NewTime(uptime)
				ns.SessionUpSince = &t
			}
		} else {
			// Populate LastError from disconnect reason for non-Established neighbors
			if peer.State.DisconnectMessage != "" {
				ns.LastError = peer.State.DisconnectMessage
			} else if peer.State.DisconnectReason != gobgpapi.PeerState_DISCONNECT_REASON_UNSPECIFIED {
				ns.LastError = peer.State.DisconnectReason.String()
			}
		}

		// Prefix counts from AfiSafi state
		for _, afiSafi := range peer.AfiSafis {
			if afiSafi.State != nil {
				ns.PrefixesSent += afiSafi.State.Advertised
				ns.PrefixesReceived += afiSafi.State.Received
			}
		}

		neighbors = append(neighbors, ns)
	}

	return neighbors, nil
}

func (r *BGPNodeStatusReporter) collectNetlinkImportStatus(ctx context.Context, apiClient GoBGPNodeStatusClient, nlClient NetlinkClient, log logr.Logger) (*bgpv1.NetlinkImportStatus, error) {
	netlinkResp, err := apiClient.GetNetlink(ctx, &gobgpapi.GetNetlinkRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetNetlink: %w", err)
	}

	status := &bgpv1.NetlinkImportStatus{
		Enabled: netlinkResp.ImportEnabled,
		VRF:     netlinkResp.Vrf,
	}

	// Collect interface states
	for _, ifName := range netlinkResp.Interfaces {
		ifStatus := bgpv1.ImportInterfaceStatus{Name: ifName}
		link, err := nlClient.LinkByName(ifName)
		if err != nil {
			ifStatus.Exists = false
			ifStatus.OperState = "down"
		} else {
			ifStatus.Exists = true
			ifStatus.OperState = interfaceOperState(link)
		}
		status.Interfaces = append(status.Interfaces, ifStatus)
	}

	// Collect imported addresses using Path.IsNetlink from ListPath
	families := defaultFamilies()
	var importedAddresses []bgpv1.ImportedAddress
	for _, family := range families {
		stream, err := apiClient.ListPath(ctx, &gobgpapi.ListPathRequest{
			TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL,
			Family:    family,
		})
		if err != nil {
			log.V(1).Info("Failed to ListPath for import status", "family", familyToString(family), "error", err)
			continue
		}
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			if resp.Destination == nil {
				continue
			}
			for _, path := range resp.Destination.Paths {
				if path.GetNetlink().GetIsNetlink() {
					importedAddresses = append(importedAddresses, bgpv1.ImportedAddress{
						Address:   resp.Destination.Prefix,
						Interface: path.GetNetlink().GetIfName(),
						InRIB:     true,
					})
				}
			}
		}
	}

	// Also add addresses from interfaces that are NOT in the RIB (for display).
	// Use the actual prefix length from the interface (e.g. /24), not /32.
	// GoBGP imports routes with the prefix length as assigned on the interface.
	nlImportedPrefixes := make(map[string]bool)
	for _, addr := range importedAddresses {
		nlImportedPrefixes[addr.Address] = true
	}
	for _, ifStatus := range status.Interfaces {
		if !ifStatus.Exists {
			continue
		}
		link, err := nlClient.LinkByName(ifStatus.Name)
		if err != nil {
			continue
		}
		addrs, err := nlClient.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			// Filter link-local
			if addr.IP.IsLinkLocalUnicast() || addr.IP.IsLinkLocalMulticast() {
				continue
			}
			if addr.IPNet == nil {
				continue
			}
			prefix := addr.IPNet.String()
			if !nlImportedPrefixes[prefix] {
				importedAddresses = append(importedAddresses, bgpv1.ImportedAddress{
					Address:   prefix,
					Interface: ifStatus.Name,
					InRIB:     false,
				})
			}
		}
	}

	status.TotalImported = len(importedAddresses)
	if len(importedAddresses) > maxImportedAddresses {
		importedAddresses = importedAddresses[:maxImportedAddresses]
		status.Truncated = true
	}
	status.ImportedAddresses = importedAddresses

	return status, nil
}

func (r *BGPNodeStatusReporter) collectRIBStatus(ctx context.Context, apiClient GoBGPNodeStatusClient, neighbors []bgpv1.NeighborStatus) (*bgpv1.RIBStatus, error) {
	status := &bgpv1.RIBStatus{}
	families := defaultFamilies()

	var localRoutes []bgpv1.RIBRoute
	var receivedRoutes []bgpv1.RIBRoute

	for _, family := range families {
		stream, err := apiClient.ListPath(ctx, &gobgpapi.ListPathRequest{
			TableType: gobgpapi.TableType_TABLE_TYPE_GLOBAL,
			Family:    family,
		})
		if err != nil {
			continue
		}

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			if resp.Destination == nil {
				continue
			}

			for _, path := range resp.Destination.Paths {
				if path.GetIsNexthopInvalid() {
					continue
				}

				// Netlink-imported routes have Netlink.IsNetlink=true, Best=false,
				// and NeighborIp="0.0.0.1" (GoBGP synthetic peer). Use
				// IsNetlink as the primary classifier, not Best or NeighborIp.
				if path.GetNetlink().GetIsNetlink() {
					localRoutes = append(localRoutes, bgpv1.RIBRoute{
						Prefix:  resp.Destination.Prefix,
						NextHop: "0.0.0.0",
					})
					continue
				}

				// Non-netlink routes: only include best paths
				if !path.GetBest() {
					continue
				}

				if path.GetNeighborIp() == "" {
					// Other locally originated routes (e.g. AddPath API)
					localRoutes = append(localRoutes, bgpv1.RIBRoute{
						Prefix:  resp.Destination.Prefix,
						NextHop: "0.0.0.0",
					})
				} else {
					// Received from a peer
					receivedRoutes = append(receivedRoutes, bgpv1.RIBRoute{
						Prefix:   resp.Destination.Prefix,
						NextHop:  getNextHopFromPath(path),
						FromPeer: path.GetNeighborIp(),
					})
				}
			}
		}
	}

	// Build advertisedTo for local routes by querying adj-out per established neighbor.
	// This is O(neighbors x adj-out-size), efficient for LoadBalancer use cases.
	if len(localRoutes) > 0 {
		localPrefixIndex := make(map[string]int, len(localRoutes))
		for i, route := range localRoutes {
			localPrefixIndex[route.Prefix] = i
		}

		// Counted separately from the slice length, because the slice stops
		// growing at maxAdvertisedTo while the count must stay accurate.
		advertisedToCount := make([]int, len(localRoutes))

		// Sorted so that *which* addresses survive the cap is deterministic.
		// ListPeer's order is not guaranteed, and without this a node with more
		// than maxAdvertisedTo established neighbors would keep the same count
		// but a different sample each poll - making statusEqual false forever
		// and turning the change-suppression path into a write every cycle.
		ordered := make([]bgpv1.NeighborStatus, len(neighbors))
		copy(ordered, neighbors)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Address < ordered[j].Address })

		for _, neighbor := range ordered {
			if neighbor.State != "Established" {
				continue
			}
			adjOutPrefixes := r.getAdjOutPrefixes(ctx, apiClient, neighbor.Address, families)
			for prefix := range adjOutPrefixes {
				if idx, ok := localPrefixIndex[prefix]; ok {
					advertisedToCount[idx]++
					if len(localRoutes[idx].AdvertisedTo) < maxAdvertisedTo {
						localRoutes[idx].AdvertisedTo = append(localRoutes[idx].AdvertisedTo, neighbor.Address)
					}
				}
			}
		}

		for i := range localRoutes {
			localRoutes[i].AdvertisedToCount = advertisedToCount[i]
			if advertisedToCount[i] > len(localRoutes[i].AdvertisedTo) {
				status.Truncated = true
			}
		}
	}

	status.LocalRouteCount = len(localRoutes)
	status.ReceivedRouteCount = len(receivedRoutes)

	if len(localRoutes) > maxLocalRoutes {
		localRoutes = localRoutes[:maxLocalRoutes]
		status.Truncated = true
	}
	if len(receivedRoutes) > maxReceivedRoutes {
		receivedRoutes = receivedRoutes[:maxReceivedRoutes]
		status.Truncated = true
	}

	status.LocalRoutes = localRoutes
	status.ReceivedRoutes = receivedRoutes

	return status, nil
}

// getAdjOutPrefixes returns the set of prefixes advertised to a specific neighbor.
func (r *BGPNodeStatusReporter) getAdjOutPrefixes(ctx context.Context, apiClient GoBGPNodeStatusClient, neighborAddr string, families []*gobgpapi.Family) map[string]bool {
	prefixes := make(map[string]bool)
	for _, family := range families {
		stream, err := apiClient.ListPath(ctx, &gobgpapi.ListPathRequest{
			TableType: gobgpapi.TableType_TABLE_TYPE_ADJ_OUT,
			Name:      neighborAddr,
			Family:    family,
		})
		if err != nil {
			continue
		}
		for {
			resp, err := stream.Recv()
			if err != nil {
				break
			}
			if resp.Destination != nil {
				prefixes[resp.Destination.Prefix] = true
			}
		}
	}
	return prefixes
}

func (r *BGPNodeStatusReporter) collectNetlinkExportStatus(ctx context.Context, apiClient GoBGPNodeStatusClient, nlClient NetlinkClient, log logr.Logger) (*bgpv1.NetlinkExportStatus, error) {
	netlinkResp, err := apiClient.GetNetlink(ctx, &gobgpapi.GetNetlinkRequest{})
	if err != nil {
		return nil, fmt.Errorf("GetNetlink: %w", err)
	}

	status := &bgpv1.NetlinkExportStatus{
		Enabled: netlinkResp.ExportEnabled,
	}

	if !netlinkResp.ExportEnabled {
		return status, nil
	}

	// Get export rules
	rulesResp, err := apiClient.ListNetlinkExportRules(ctx, &gobgpapi.ListNetlinkExportRulesRequest{})
	if err != nil {
		log.V(1).Info("Failed to get export rules", "error", err)
	} else if rulesResp != nil {
		for _, rule := range rulesResp.Rules {
			status.Rules = append(status.Rules, bgpv1.ExportRuleStatus{
				Name:    rule.Name,
				Metric:  rule.Metric,
				TableID: rule.TableId,
			})
		}
	}

	// Get exported routes from GoBGP (authoritative)
	var exportedRoutes []bgpv1.ExportedRoute
	exportStream, err := apiClient.ListNetlinkExport(ctx, &gobgpapi.ListNetlinkExportRequest{})
	if err != nil {
		log.V(1).Info("Failed to list netlink exports", "error", err)
	} else {
		// Build kernel route set for cross-check
		kernelRoutes := r.getKernelRoutes(nlClient, log)

		for {
			resp, err := exportStream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				break
			}
			if resp.Route == nil {
				continue
			}

			route := bgpv1.ExportedRoute{
				Prefix:    resp.Route.Prefix,
				Table:     tableIDToName(resp.Route.TableId),
				Metric:    resp.Route.Metric,
				Installed: kernelRoutes[resp.Route.Prefix],
			}
			if !route.Installed {
				route.Reason = "not found in kernel routing table"
			}
			exportedRoutes = append(exportedRoutes, route)
		}
	}

	status.TotalExported = len(exportedRoutes)
	if len(exportedRoutes) > maxExportedRoutes {
		exportedRoutes = exportedRoutes[:maxExportedRoutes]
		status.Truncated = true
	}
	status.ExportedRoutes = exportedRoutes

	return status, nil
}

func (r *BGPNodeStatusReporter) collectVRFStatus(ctx context.Context, apiClient GoBGPNodeStatusClient) ([]bgpv1.VRFStatus, error) {
	stream, err := apiClient.ListVrf(ctx, &gobgpapi.ListVrfRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListVrf: %w", err)
	}

	var vrfs []bgpv1.VRFStatus
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return vrfs, fmt.Errorf("ListVrf stream: %w", err)
		}
		if resp.Vrf == nil {
			continue
		}

		vrfStatus := bgpv1.VRFStatus{
			Name: resp.Vrf.Name,
			RD:   formatRouteDistinguisher(resp.Vrf.Rd),
		}

		// Get imported route count via GetTable (cheap unary RPC, not streaming)
		for _, family := range defaultFamilies() {
			tableResp, tableErr := apiClient.GetTable(ctx, &gobgpapi.GetTableRequest{
				TableType: gobgpapi.TableType_TABLE_TYPE_VRF,
				Name:      resp.Vrf.Name,
				Family:    family,
			})
			if tableErr == nil {
				// #nosec G115 -- NumPath is a route count that fits in int on all platforms
				vrfStatus.ImportedRouteCount += int(tableResp.NumPath)
			}
		}

		// Get exported route count via ListNetlinkExport filtered by VRF
		exportStream, err := apiClient.ListNetlinkExport(ctx, &gobgpapi.ListNetlinkExportRequest{
			Vrf: resp.Vrf.Name,
		})
		if err == nil {
			for {
				_, recvErr := exportStream.Recv()
				if recvErr != nil {
					break
				}
				vrfStatus.ExportedRouteCount++
			}
		}

		vrfs = append(vrfs, vrfStatus)
	}

	return vrfs, nil
}

// writeStatus writes the status to the BGPNodeStatus CRD if changed or heartbeat elapsed.
func (r *BGPNodeStatusReporter) writeStatus(ctx context.Context, status *bgpv1.BGPNodeStatusData, log logr.Logger) error {
	if r.statusEqual(r.lastWrittenStatus, status) {
		elapsed := time.Since(r.lastWriteTime)
		heartbeat := time.Duration(r.heartbeatSeconds.Load()) * time.Second
		if elapsed < heartbeat {
			log.V(1).Info("Status unchanged, skipping write", "nextHeartbeat", heartbeat-elapsed)
			nodeStatusWriteTotal.WithLabelValues("skipped").Inc()
			return nil
		}
	} else {
		// Change-driven write: add jitter to prevent write storms
		heartbeat := r.heartbeatSeconds.Load()
		jitterMax := heartbeat / 4
		if jitterMax > 0 {
			jitter := time.Duration(rand.Intn(int(jitterMax))) * time.Second // #nosec G404 -- jitter timing, not security
			time.Sleep(jitter)
		}
	}

	// Set metadata
	now := metav1.Now()
	status.LastUpdated = &now
	status.HeartbeatSeconds = r.heartbeatSeconds.Load()

	// Get existing object, set status, and update.
	// We use Status().Update() (not Patch) because MergePatch cannot add
	// zero-valued required fields that were previously absent. Each node
	// has a single writer so ResourceVersion conflicts don't occur.
	existing := &bgpv1.BGPNodeStatus{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: r.NodeName}, existing); err != nil {
		nodeStatusWriteTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("get BGPNodeStatus for update: %w", err)
	}

	existing.Status = *status

	if err := r.Client.Status().Update(ctx, existing); err != nil {
		nodeStatusWriteTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("update BGPNodeStatus status: %w", err)
	}

	nodeStatusWriteTotal.WithLabelValues("success").Inc()
	nodeStatusLastSuccessfulWrite.SetToCurrentTime()

	// Estimate object size
	if data, err := json.Marshal(status); err == nil {
		nodeStatusObjectSizeBytes.Set(float64(len(data)))
	}

	r.lastWrittenStatus = status.DeepCopy()
	r.lastWriteTime = time.Now()
	return nil
}

// bfdDegraded reports whether BFD is configured on this node but not working.
//
// The second return is the condition reason, and the third the message; both are
// empty when BFD is not in use at all. That "not in use" case is the important
// one: deriveHealth is a pure AND, and the BFD socket binds lazily on the first
// BFD peer, so treating `listening == false` as unhealthy without this gate
// would mark every non-BFD node - including every local-address-mode deployment,
// which peers with nobody - permanently unhealthy.
func (r *BGPNodeStatusReporter) bfdDegraded(status *bgpv1.BGPNodeStatusData) (bool, string, string) {
	configured := 0
	down := 0
	for _, n := range status.Neighbors {
		if n.BFD == nil {
			continue
		}
		configured++
		if n.BFD.SessionState != "Up" {
			down++
		}
	}
	if configured == 0 {
		return false, "", ""
	}

	if status.BFDServer == nil || !status.BFDServer.Listening {
		return true, "ServerNotListening", fmt.Sprintf(
			"%d neighbor(s) have BFD enabled but the BFD socket is not bound; no failure detection is running", configured)
	}
	if down > 0 {
		// Usually a link-local neighborAddress configured without its %zone:
		// transmit succeeds, BGP establishes, BFD never leaves Down. Briefly
		// also true while a newly added session comes up.
		return true, "SessionsDown", fmt.Sprintf("%d of %d BFD session(s) are not Up", down, configured)
	}
	return false, "Healthy", fmt.Sprintf("%d BFD session(s) Up", configured)
}

// deriveHealth returns true if all neighbors are Established and no import/export failures.
func (r *BGPNodeStatusReporter) deriveHealth(status *bgpv1.BGPNodeStatusData) bool {
	for _, n := range status.Neighbors {
		if n.State != "Established" {
			return false
		}
	}
	if degraded, _, _ := r.bfdDegraded(status); degraded {
		return false
	}
	if status.NetlinkImport != nil {
		for _, addr := range status.NetlinkImport.ImportedAddresses {
			if !addr.InRIB {
				return false
			}
		}
	}
	if status.NetlinkExport != nil {
		for _, route := range status.NetlinkExport.ExportedRoutes {
			if !route.Installed {
				return false
			}
		}
	}
	return true
}

func (r *BGPNodeStatusReporter) setReadyCondition(status *bgpv1.BGPNodeStatusData) {
	if status.Healthy {
		established := 0
		for _, n := range status.Neighbors {
			if n.State == "Established" {
				established++
			}
		}
		msg := fmt.Sprintf("%d neighbor(s) established", established)
		if status.NetlinkImport != nil && status.NetlinkImport.Enabled {
			msg += ", netlinkImport active"
		}
		setCondition(status, "Ready", metav1.ConditionTrue, "AllHealthy", msg)
	} else {
		setCondition(status, "Ready", metav1.ConditionFalse, "Degraded", "one or more components unhealthy")
	}

	// BFDDegraded is reported only on nodes that actually use BFD, so a
	// non-BFD node carries no BFD condition at all rather than a permanent
	// False one that reads like a warning.
	if degraded, reason, msg := r.bfdDegraded(status); reason != "" {
		condStatus := metav1.ConditionFalse
		if degraded {
			condStatus = metav1.ConditionTrue
		}
		setCondition(status, "BFDDegraded", condStatus, reason, msg)
	}
}

// statusEqual compares two status snapshots, normalizing nil/empty slices and excluding timestamps.
func (r *BGPNodeStatusReporter) statusEqual(a, b *bgpv1.BGPNodeStatusData) bool {
	if a == nil || b == nil {
		return a == b
	}

	if a.NodeName != b.NodeName || a.RouterID != b.RouterID || a.RouterIDSource != b.RouterIDSource ||
		a.ASN != b.ASN || a.NeighborCount != b.NeighborCount || a.Healthy != b.Healthy {
		return false
	}

	if !neighborsEqual(a.Neighbors, b.Neighbors) {
		return false
	}
	if !importStatusEqual(a.NetlinkImport, b.NetlinkImport) {
		return false
	}
	if !ribStatusEqual(a.RIB, b.RIB) {
		return false
	}
	if !exportStatusEqual(a.NetlinkExport, b.NetlinkExport) {
		return false
	}
	if !vrfsEqual(a.VRFs, b.VRFs) {
		return false
	}

	return true
}

// --- Helpers ---

func setCondition(status *bgpv1.BGPNodeStatusData, condType string, condStatus metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	for i, c := range status.Conditions {
		if c.Type == condType {
			status.Conditions[i].Status = condStatus
			status.Conditions[i].Reason = reason
			status.Conditions[i].Message = message
			if c.Status != condStatus {
				status.Conditions[i].LastTransitionTime = now
			}
			return
		}
	}
	status.Conditions = append(status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// bfdStateToString renders a BFD session state for the CR.
//
// The CR's vocabulary, not the protobuf enum's: NeighborStatus.State already
// reads "Established", so emitting "BFD_SESSION_STATE_UP" beside it would put
// two conventions in one object. Metric labels keep the enum names - that is a
// third vocabulary, and docs/metrics.md maps between them.
func bfdStateToString(s gobgpapi.BfdSessionState) string {
	switch s {
	case gobgpapi.BfdSessionState_BFD_SESSION_STATE_UP:
		return "Up"
	case gobgpapi.BfdSessionState_BFD_SESSION_STATE_DOWN:
		return "Down"
	case gobgpapi.BfdSessionState_BFD_SESSION_STATE_ADMIN_DOWN:
		return "AdminDown"
	case gobgpapi.BfdSessionState_BFD_SESSION_STATE_INIT:
		return "Init"
	default:
		return "Unspecified"
	}
}

// bfdDiagToString renders an RFC 5880 diagnostic code. Code 0 is
// "NoDiagnostic", a healthy value rather than an absent one.
func bfdDiagToString(d gobgpapi.BfdDiagnosticCode) string {
	switch d {
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_NO_DIAGNOSTIC:
		return "NoDiagnostic"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_DETECTION_TIMEOUT:
		return "DetectionTimeout"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_ECHO_FAILED:
		return "EchoFailed"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_NEIGHBOR_SIGNALED_SESSION_DOWN:
		return "NeighborSignaledSessionDown"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_FORWARDING_PLANE_RESET:
		return "ForwardingPlaneReset"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_PATH_DOWN:
		return "PathDown"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_CONCATENATED_PATH_DOWN:
		return "ConcatenatedPathDown"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_ADMINISTRATIVELY_DOWN:
		return "AdministrativelyDown"
	case gobgpapi.BfdDiagnosticCode_BFD_DIAGNOSTIC_CODE_REVERSE_CONCATENATED_PATH_DOWN:
		return "ReverseConcatenatedPathDown"
	default:
		return "Unknown"
	}
}

func peerStateToString(state gobgpapi.PeerState_SessionState) string {
	switch state {
	case gobgpapi.PeerState_SESSION_STATE_IDLE:
		return "Idle"
	case gobgpapi.PeerState_SESSION_STATE_CONNECT:
		return "Connect"
	case gobgpapi.PeerState_SESSION_STATE_ACTIVE:
		return "Active"
	case gobgpapi.PeerState_SESSION_STATE_OPENSENT:
		return "OpenSent"
	case gobgpapi.PeerState_SESSION_STATE_OPENCONFIRM:
		return "OpenConfirm"
	case gobgpapi.PeerState_SESSION_STATE_ESTABLISHED:
		return "Established"
	default:
		return "Unknown"
	}
}

func getNextHopFromPath(path *gobgpapi.Path) string {
	// The nexthop is typically in the path attributes
	// For our purposes, the NeighborIp serves as a reasonable proxy
	if nh := path.GetNeighborIp(); nh != "" {
		return nh
	}
	return "0.0.0.0"
}

func addrToHostPrefix(addr netlink.Addr) string {
	if addr.IP.To4() != nil {
		return fmt.Sprintf("%s/32", addr.IP.String())
	}
	return fmt.Sprintf("%s/128", addr.IP.String())
}

func tableIDToName(id int32) string {
	switch id {
	case 0, 254:
		return "main"
	case 253:
		return "default"
	case 255:
		return "local"
	default:
		return fmt.Sprintf("%d", id)
	}
}

// formatRouteDistinguisher converts a protobuf RouteDistinguisher to a "ASN:value" string.
func formatRouteDistinguisher(rd *gobgpapi.RouteDistinguisher) string {
	if rd == nil {
		return ""
	}
	switch v := rd.Rd.(type) {
	case *gobgpapi.RouteDistinguisher_TwoOctetAsn:
		return fmt.Sprintf("%d:%d", v.TwoOctetAsn.Admin, v.TwoOctetAsn.Assigned)
	case *gobgpapi.RouteDistinguisher_IpAddress:
		return fmt.Sprintf("%s:%d", v.IpAddress.Admin, v.IpAddress.Assigned)
	case *gobgpapi.RouteDistinguisher_FourOctetAsn:
		return fmt.Sprintf("%d:%d", v.FourOctetAsn.Admin, v.FourOctetAsn.Assigned)
	default:
		return ""
	}
}

func (r *BGPNodeStatusReporter) getKernelRoutes(nlClient NetlinkClient, log logr.Logger) map[string]bool {
	routes := make(map[string]bool)
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		filter := &netlink.Route{Protocol: 186} // RTPROT_BGP
		routeList, err := nlClient.RouteListFiltered(family, filter, netlink.RT_FILTER_PROTOCOL)
		if err != nil {
			log.V(1).Info("Failed to list kernel routes", "family", family, "error", err)
			continue
		}
		for _, route := range routeList {
			if route.Dst != nil {
				routes[route.Dst.String()] = true
			}
		}
	}
	return routes
}

// --- Deep-equal helpers (normalize nil/empty slices) ---

func sliceEmpty[T any](s []T) bool {
	return len(s) == 0
}

func timeEqual(a, b *metav1.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(b)
}

func bfdStatusEqual(a, b *bgpv1.BFDStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SessionState == b.SessionState &&
		a.RemoteSessionState == b.RemoteSessionState &&
		a.LocalDiagnosticCode == b.LocalDiagnosticCode &&
		a.RemoteDiagnosticCode == b.RemoteDiagnosticCode &&
		a.FailureTransitions == b.FailureTransitions
}

func neighborsEqual(a, b []bgpv1.NeighborStatus) bool {
	if sliceEmpty(a) && sliceEmpty(b) {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	// Sort by address for stable comparison
	sortedA := make([]bgpv1.NeighborStatus, len(a))
	sortedB := make([]bgpv1.NeighborStatus, len(b))
	copy(sortedA, a)
	copy(sortedB, b)
	sort.Slice(sortedA, func(i, j int) bool { return sortedA[i].Address < sortedA[j].Address })
	sort.Slice(sortedB, func(i, j int) bool { return sortedB[i].Address < sortedB[j].Address })

	for i := range sortedA {
		na, nb := sortedA[i], sortedB[i]
		if na.Address != nb.Address || na.PeerASN != nb.PeerASN || na.LocalASN != nb.LocalASN ||
			na.State != nb.State || na.PrefixesSent != nb.PrefixesSent ||
			na.PrefixesReceived != nb.PrefixesReceived || na.Description != nb.Description ||
			na.LastError != nb.LastError {
			return false
		}
		// SessionUpSince was already missing from this list, so a session that
		// went down and came back between two collections looked unchanged.
		if !timeEqual(na.SessionUpSince, nb.SessionUpSince) {
			return false
		}
		// This list is an allow-list: a field not named here is invisible to the
		// writer, so a BFD session dropping while BGP stays up would never reach
		// the CR at all.
		if !bfdStatusEqual(na.BFD, nb.BFD) {
			return false
		}
	}
	return true
}

func importStatusEqual(a, b *bgpv1.NetlinkImportStatus) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Enabled != b.Enabled || a.VRF != b.VRF || a.TotalImported != b.TotalImported || a.Truncated != b.Truncated {
		return false
	}
	if len(a.Interfaces) != len(b.Interfaces) {
		return false
	}
	for i := range a.Interfaces {
		if a.Interfaces[i] != b.Interfaces[i] {
			return false
		}
	}
	if len(a.ImportedAddresses) != len(b.ImportedAddresses) {
		return false
	}
	for i := range a.ImportedAddresses {
		if a.ImportedAddresses[i] != b.ImportedAddresses[i] {
			return false
		}
	}
	return true
}

func ribStatusEqual(a, b *bgpv1.RIBStatus) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.LocalRouteCount != b.LocalRouteCount || a.ReceivedRouteCount != b.ReceivedRouteCount || a.Truncated != b.Truncated {
		return false
	}
	if !ribRoutesEqual(a.LocalRoutes, b.LocalRoutes) {
		return false
	}
	return ribRoutesEqual(a.ReceivedRoutes, b.ReceivedRoutes)
}

func ribRoutesEqual(a, b []bgpv1.RIBRoute) bool {
	if sliceEmpty(a) && sliceEmpty(b) {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	sortedA := make([]bgpv1.RIBRoute, len(a))
	sortedB := make([]bgpv1.RIBRoute, len(b))
	copy(sortedA, a)
	copy(sortedB, b)
	sort.Slice(sortedA, func(i, j int) bool { return sortedA[i].Prefix < sortedA[j].Prefix })
	sort.Slice(sortedB, func(i, j int) bool { return sortedB[i].Prefix < sortedB[j].Prefix })

	for i := range sortedA {
		if sortedA[i].Prefix != sortedB[i].Prefix || sortedA[i].NextHop != sortedB[i].NextHop ||
			sortedA[i].FromPeer != sortedB[i].FromPeer {
			return false
		}
		if sortedA[i].AdvertisedToCount != sortedB[i].AdvertisedToCount {
			return false
		}
		if !stringSlicesEqual(sortedA[i].AdvertisedTo, sortedB[i].AdvertisedTo) {
			return false
		}
		if !stringSlicesEqual(sortedA[i].Communities, sortedB[i].Communities) {
			return false
		}
	}
	return true
}

func exportStatusEqual(a, b *bgpv1.NetlinkExportStatus) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Enabled != b.Enabled || a.Protocol != b.Protocol ||
		a.TotalExported != b.TotalExported || a.Truncated != b.Truncated {
		return false
	}
	if len(a.Rules) != len(b.Rules) {
		return false
	}
	for i := range a.Rules {
		if a.Rules[i] != b.Rules[i] {
			return false
		}
	}
	if len(a.ExportedRoutes) != len(b.ExportedRoutes) {
		return false
	}
	for i := range a.ExportedRoutes {
		if a.ExportedRoutes[i] != b.ExportedRoutes[i] {
			return false
		}
	}
	return true
}

func vrfsEqual(a, b []bgpv1.VRFStatus) bool {
	if sliceEmpty(a) && sliceEmpty(b) {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if sliceEmpty(a) && sliceEmpty(b) {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	sortedA := make([]string, len(a))
	sortedB := make([]string, len(b))
	copy(sortedA, a)
	copy(sortedB, b)
	sort.Strings(sortedA)
	sort.Strings(sortedB)
	for i := range sortedA {
		if sortedA[i] != sortedB[i] {
			return false
		}
	}
	return true
}

// sanitizeLabelValue and isLinkLocal already defined in other controller files.
