// Package tenstorrent implements profiles.Profile for Wormhole hardware.
// It contacts the per-node Fabric Manager agent via gRPC to discover the
// physical topology, groups MMIO and non-MMIO ASICs into per-tray bundles,
// and publishes a single aggregated device per node that carries the full
// topology as ResourceSlice attributes along with a memory capacity field.
package tenstorrent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/tenstorrent/wh-dra-plugin/internal/fabricmanager"
	topologypb "github.com/tenstorrent/wh-dra-plugin/internal/fabricmanager/proto/topology"
)

// deviceName is the stable name of the aggregate device advertised in the
// ResourceSlice. One device per node represents the full T3K (all chips).
const deviceName = "wormhole-t3k"

// hostBundle groups a single MMIO-capable ASIC with its non-MMIO siblings
// on the same physical tray. The MMIO chip controls the /dev/tenstorrent/N
// entry; non-MMIO chips are accessible through that fd.
type hostBundle struct {
	mmio    *topologypb.AsicInfo
	remotes []*topologypb.AsicInfo
}

// Profile enumerates Wormhole ASIC devices via the Fabric Manager agent and
// exposes them as a single aggregate ResourceSlice device with rich topology
// attributes and memory capacity.
type Profile struct {
	nodeName      string
	topology      fabricmanager.TopologyClient
	arch          string
	boardType     string
	physicalPod   string
	hostRank      int
	podSize       int
	ethernetIface string

	mu          sync.RWMutex
	devicePaths []string // /dev/tenstorrent/<deviceNodeId> per MMIO ASIC
}

// New constructs a Tenstorrent profile. The label-derived fields (arch,
// boardType, etc.) are folded into the ResourceSlice attributes alongside
// FM-discovered attributes so workload selectors can match on either source.
func New(
	nodeName string,
	topology fabricmanager.TopologyClient,
	arch, boardType, physicalPod, ethernetIface string,
	hostRank, podSize int,
) *Profile {
	return &Profile{
		nodeName:      nodeName,
		topology:      topology,
		arch:          arch,
		boardType:     boardType,
		physicalPod:   physicalPod,
		hostRank:      hostRank,
		podSize:       podSize,
		ethernetIface: ethernetIface,
	}
}

// EnumerateDevices implements profiles.Profile. It calls the FM agent,
// bundles ASICs into MMIO-led groups, and publishes one aggregate device
// named "wormhole-t3k" carrying:
//   - label-based attributes (arch, board_type, physical_pod, etc.)
//   - FM-sourced attributes (chipArch, uniqueIDs, mmioChipCount,
//     remoteChipCount, pciAddresses, remoteChipIDs, remoteUniqueIDs)
//   - Device.Capacity["memory"] = total DRAM across all ASICs
func (p *Profile) EnumerateDevices(ctx context.Context) (resourceslice.DriverResources, error) {
	topo, err := p.topology.GetTopology(ctx)
	if err != nil {
		return resourceslice.DriverResources{}, fmt.Errorf("tenstorrent profile: get FM topology: %w", err)
	}

	bundles := bundleHostASICs(topo.GetAsics())
	if len(bundles) == 0 {
		return resourceslice.DriverResources{}, fmt.Errorf("tenstorrent profile: FM reported no MMIO-capable ASICs on node %s", p.nodeName)
	}

	device, paths := p.buildDevice(bundles)

	p.mu.Lock()
	p.devicePaths = paths
	p.mu.Unlock()

	memQ := device.Capacity["tenstorrent.com/memory"].Value
	klog.V(2).Infof("tenstorrent profile: node %s — %d bundles, %d MMIO paths, memory=%s",
		p.nodeName, len(bundles), len(paths), memQ.String())

	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			p.nodeName: {
				Slices: []resourceslice.Slice{{Devices: []resourceapi.Device{device}}},
			},
		},
	}, nil
}

// DeviceNodePaths implements profiles.Profile. For the aggregate device
// "wormhole-t3k" it returns the /dev/tenstorrent/<N> path of every
// MMIO-capable ASIC discovered in the last EnumerateDevices call.
func (p *Profile) DeviceNodePaths(name string) ([]string, error) {
	if name != deviceName {
		return nil, fmt.Errorf("tenstorrent profile: unknown device %q (only %q is published)", name, deviceName)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.devicePaths) == 0 {
		return nil, fmt.Errorf("tenstorrent profile: EnumerateDevices has not run yet")
	}
	out := make([]string, len(p.devicePaths))
	copy(out, p.devicePaths)
	return out, nil
}

// buildDevice constructs the aggregate ResourceSlice device from the list of
// bundles and returns the corresponding /dev paths.
func (p *Profile) buildDevice(bundles []hostBundle) (resourceapi.Device, []string) {
	var (
		uniqueIDs     []string
		remoteChipIDs []string
		remoteUIDs    []string
		pciAddrs      []string
		paths         []string
		totalMem      uint64
		mmioCount     int
		remoteCount   int
		chipArch      string
	)

	for _, b := range bundles {
		mmio := b.mmio
		mmioCount++
		totalMem += mmio.GetMemoryBytes()
		uniqueIDs = append(uniqueIDs, fmt.Sprintf("%d", mmio.GetUniqueId()))
		paths = append(paths, fmt.Sprintf("/dev/tenstorrent/%d", mmio.GetDeviceNodeId()))

		if chipArch == "" {
			chipArch = mmio.GetChipArch()
		}
		if pci := mmio.GetPciAddress(); pci != "" {
			pciAddrs = append(pciAddrs, pci)
		}

		for _, r := range b.remotes {
			remoteCount++
			totalMem += r.GetMemoryBytes()
			uniqueIDs = append(uniqueIDs, fmt.Sprintf("%d", r.GetUniqueId()))
			remoteChipIDs = append(remoteChipIDs, fmt.Sprintf("%d", r.GetChipId()))
			remoteUIDs = append(remoteUIDs, fmt.Sprintf("%d", r.GetUniqueId()))
		}
	}

	totalChips := int64(mmioCount + remoteCount)

	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		// Label-derived (workload selectors may match on these)
		"tenstorrent.com/arch":         {StringValue: ptrS(p.arch)},
		"tenstorrent.com/board_type":   {StringValue: ptrS(p.boardType)},
		"tenstorrent.com/physical_pod": {StringValue: ptrS(p.physicalPod)},
		"tenstorrent.com/host_rank":    {IntValue: ptrI(int64(p.hostRank))},
		"tenstorrent.com/pod_size":     {IntValue: ptrI(int64(p.podSize))},
		"tenstorrent.com/chip_count":   {IntValue: ptrI(totalChips)},
		// FM-derived
		"tenstorrent.com/chip_arch":        {StringValue: ptrS(chipArch)},
		"tenstorrent.com/mmio_chip_count":  {IntValue: ptrI(int64(mmioCount))},
		"tenstorrent.com/remote_chip_count": {IntValue: ptrI(int64(remoteCount))},
		"tenstorrent.com/unique_ids":        {StringValue: ptrS(strings.Join(uniqueIDs, ","))},
	}
	if len(remoteChipIDs) > 0 {
		attrs["tenstorrent.com/remote_chip_ids"] = resourceapi.DeviceAttribute{StringValue: ptrS(strings.Join(remoteChipIDs, ","))}
		attrs["tenstorrent.com/remote_unique_ids"] = resourceapi.DeviceAttribute{StringValue: ptrS(strings.Join(remoteUIDs, ","))}
	}
	if len(pciAddrs) > 0 {
		attrs["tenstorrent.com/pci_addresses"] = resourceapi.DeviceAttribute{StringValue: ptrS(strings.Join(pciAddrs, ","))}
	}
	if p.ethernetIface != "" {
		attrs["tenstorrent.com/ethernet_iface"] = resourceapi.DeviceAttribute{StringValue: ptrS(p.ethernetIface)}
	}

	device := resourceapi.Device{
		Name:       deviceName,
		Attributes: attrs,
	}
	if totalMem > 0 {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"tenstorrent.com/memory": {
				Value: *resource.NewQuantity(int64(totalMem), resource.BinarySI),
			},
		}
	}
	return device, paths
}

// bundleHostASICs partitions the ASIC list into per-tray bundles. Within each
// tray, the MMIO chip with the lowest asicLocation leads the bundle and all
// non-MMIO chips on that tray become its remotes. Additional MMIO chips on the
// same tray each become their own bundle with no remotes.
func bundleHostASICs(asics []*topologypb.AsicInfo) []hostBundle {
	byTray := make(map[uint32][]*topologypb.AsicInfo)
	for _, a := range asics {
		byTray[a.GetTrayId()] = append(byTray[a.GetTrayId()], a)
	}

	var bundles []hostBundle
	for trayID, group := range byTray {
		var mmios, remotes []*topologypb.AsicInfo
		for _, a := range group {
			if a.GetIsMmioCapable() {
				mmios = append(mmios, a)
			} else {
				remotes = append(remotes, a)
			}
		}
		if len(mmios) == 0 {
			klog.Warningf("tenstorrent profile: tray %d has no MMIO ASIC; skipping (chipIDs=%v)", trayID, chipIDsOf(remotes))
			continue
		}
		sort.Slice(mmios, func(i, j int) bool {
			return mmios[i].GetAsicLocation() < mmios[j].GetAsicLocation()
		})
		bundles = append(bundles, hostBundle{mmio: mmios[0], remotes: remotes})
		for _, extra := range mmios[1:] {
			bundles = append(bundles, hostBundle{mmio: extra})
		}
	}

	sort.Slice(bundles, func(i, j int) bool {
		return bundles[i].mmio.GetChipId() < bundles[j].mmio.GetChipId()
	})
	return bundles
}

func chipIDsOf(asics []*topologypb.AsicInfo) []uint32 {
	ids := make([]uint32, len(asics))
	for i, a := range asics {
		ids[i] = a.GetChipId()
	}
	return ids
}

func ptrS(s string) *string { return &s }
func ptrI(i int64) *int64   { return &i }
