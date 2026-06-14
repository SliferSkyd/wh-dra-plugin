package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	"github.com/tenstorrent/wh-dra-plugin/internal/fabricmanager"
	"github.com/tenstorrent/wh-dra-plugin/internal/profiles"
	tenstorrentprofile "github.com/tenstorrent/wh-dra-plugin/internal/profiles/tenstorrent"
	flockpkg "github.com/tenstorrent/wh-dra-plugin/pkg/flock"
)

const driverName = "wormhole.tenstorrent.com"

// fmRetryInterval is how often the background goroutine retries FM enrollment
// after a startup failure. Kept short so the ResourceSlice is upgraded to
// FM-sourced data quickly once the agent becomes ready.
const fmRetryInterval = 10 * time.Second

type driver struct {
	helper  *kubeletplugin.Helper
	state   *DeviceState
	manager *WHManager
	profile profiles.Profile // nil when FM integration is disabled
	healthy bool             // false → publish empty pool
	fmReady bool             // true once EnumerateDevices has succeeded at least once
}

func NewDriver(ctx context.Context, cfg *config, k8s kubernetes.Interface) (*driver, error) {
	manager, err := NewWHManager(ctx, cfg.nodeName, k8s)
	if err != nil {
		return nil, fmt.Errorf("init manager: %w", err)
	}
	klog.Infof("node %s: arch=%s board=%s chips=%d pod=%s rank=%d",
		manager.nodeName, manager.arch, manager.boardType,
		manager.chipCount, manager.physicalPod, manager.hostRank)

	// Wire FM integration when an agent address is provided.
	var prof profiles.Profile
	if cfg.fmAddr != "" {
		fmClient, err := fabricmanager.Dial(cfg.fmAddr)
		if err != nil {
			klog.Warningf("cannot connect to FM agent at %s: %v; falling back to label-based device", cfg.fmAddr, err)
		} else {
			prof = tenstorrentprofile.New(
				cfg.nodeName, fmClient,
				manager.arch, manager.boardType, manager.physicalPod, manager.ethernetIface,
				manager.hostRank, manager.podSize,
			)
			klog.Infof("FM integration enabled: agent at %s", cfg.fmAddr)
		}
	}

	cdi, err := NewCDIHandler(cfg.cdiDir, manager)
	if err != nil {
		return nil, fmt.Errorf("init CDI handler: %w", err)
	}
	if err := cdi.CreateCommonSpecFile(); err != nil {
		return nil, fmt.Errorf("write CDI common spec: %w", err)
	}

	cpManager, err := NewCheckpointManager(cfg.checkpointDir)
	if err != nil {
		return nil, fmt.Errorf("init checkpoint manager: %w", err)
	}

	pulock := flockpkg.NewFlock(filepath.Join(cfg.checkpointDir, "pu.lock"))
	state := NewDeviceState(manager, prof, cdi, cpManager, pulock, driverName)

	pluginDir := filepath.Join(cfg.pluginDir)

	helper, err := kubeletplugin.Start(ctx, state,
		kubeletplugin.DriverName(driverName),
		kubeletplugin.NodeName(cfg.nodeName),
		kubeletplugin.KubeClient(k8s),
		kubeletplugin.PluginDataDirectoryPath(pluginDir),
		kubeletplugin.RegistrarDirectoryPath(cfg.registrarDir),
		kubeletplugin.Serialize(false),
	)
	if err != nil {
		return nil, fmt.Errorf("start kubelet plugin: %w", err)
	}

	d := &driver{helper: helper, state: state, manager: manager, profile: prof, healthy: true}

	if err := d.publishResourceSlice(ctx); err != nil {
		return nil, fmt.Errorf("publish resource slice: %w", err)
	}

	// If FM wasn't ready at startup, retry in the background until it is.
	d.startFMRetry(ctx)

	return d, nil
}

func (d *driver) publishResourceSlice(ctx context.Context) error {
	var resources resourceslice.DriverResources

	switch {
	case !d.healthy:
		resources = resourceslice.DriverResources{
			Pools: map[string]resourceslice.Pool{
				d.manager.PoolName(): {
					TotalSliceCount: d.manager.PoolTotalSliceCount(),
					Slices:          nil,
				},
			},
		}
		klog.Warningf("published empty ResourceSlice on pool %s (T3K unhealthy)", d.manager.PoolName())

	case d.profile != nil:
		var err error
		resources, err = d.profile.EnumerateDevices(ctx)
		if err != nil {
			klog.Errorf("FM EnumerateDevices failed: %v; falling back to label-based device", err)
			resources = d.labelBasedResources()
		} else {
			d.fmReady = true
		}

	default:
		resources = d.labelBasedResources()
	}

	if err := d.helper.PublishResources(ctx, resources); err != nil {
		return err
	}
	return nil
}

// startFMRetry retries FM enrollment every fmRetryInterval until the first
// successful EnumerateDevices call. A no-op if FM is already ready or disabled.
func (d *driver) startFMRetry(ctx context.Context) {
	if d.profile == nil || d.fmReady {
		return
	}
	klog.Infof("FM not ready at startup, will retry every %s", fmRetryInterval)
	go func() {
		ticker := time.NewTicker(fmRetryInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if d.fmReady {
					return
				}
				if err := d.publishResourceSlice(ctx); err != nil {
					klog.Errorf("FM retry: republish failed: %v", err)
					continue
				}
				if d.fmReady {
					klog.Infof("FM retry succeeded on node %s, ResourceSlice upgraded to FM data", d.manager.nodeName)
					return
				}
			}
		}
	}()
}

// labelBasedResources builds the pre-FM single-device ResourceSlice from node
// labels only. Used when FM integration is disabled or temporarily unavailable.
func (d *driver) labelBasedResources() resourceslice.DriverResources {
	m := d.manager
	device := resourceapi.Device{
		Name: "wormhole-t3k",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"tenstorrent.com/arch":         {StringValue: ptr(m.arch)},
			"tenstorrent.com/board_type":   {StringValue: ptr(m.boardType)},
			"tenstorrent.com/physical_pod": {StringValue: ptr(m.physicalPod)},
			"tenstorrent.com/host_rank":    {IntValue: ptr(int64(m.hostRank))},
			"tenstorrent.com/pod_size":     {IntValue: ptr(int64(m.podSize))},
			"tenstorrent.com/chip_count":   {IntValue: ptr(int64(m.chipCount))},
		},
	}
	return resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			m.PoolName(): {
				TotalSliceCount: m.PoolTotalSliceCount(),
				Slices:          []resourceslice.Slice{{Devices: []resourceapi.Device{device}}},
			},
		},
	}
}

// startHealthMonitoring begins periodic tt-smi checks. When health flips it
// republishes the ResourceSlice so the scheduler sees the change immediately.
// A no-op if ttSmiPath is empty or interval is zero.
func (d *driver) startHealthMonitoring(ctx context.Context, ttSmiPath string, interval time.Duration) {
	if interval == 0 {
		klog.Info("health monitoring disabled")
		return
	}
	hc := newHealthChecker(ttSmiPath, interval, d.manager.chipCount)
	go hc.run(ctx, func(healthy bool) {
		d.healthy = healthy
		if err := d.publishResourceSlice(ctx); err != nil {
			klog.Errorf("republish ResourceSlice after health change: %v", err)
		}
	})
	klog.Infof("health monitoring started (interval=%s chips=%d)",
		interval, d.manager.chipCount)
}

func (d *driver) Stop() {
	d.helper.Stop()
}

func ptr[T any](v T) *T { return &v }
