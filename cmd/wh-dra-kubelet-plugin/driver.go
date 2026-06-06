package main

import (
	"context"
	"fmt"
	"path/filepath"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"

	flockpkg "github.com/tenstorrent/wh-dra-plugin/pkg/flock"
)

const driverName = "wormhole.tenstorrent.com"

type driver struct {
	helper  *kubeletplugin.Helper
	state   *DeviceState
	manager *WHManager
}

func NewDriver(ctx context.Context, cfg *config, k8s kubernetes.Interface) (*driver, error) {
	manager, err := NewWHManager(ctx, cfg.nodeName, k8s)
	if err != nil {
		return nil, fmt.Errorf("init manager: %w", err)
	}
	klog.Infof("node %s: arch=%s board=%s chips=%d pod=%s rank=%d",
		manager.nodeName, manager.arch, manager.boardType,
		manager.chipCount, manager.physicalPod, manager.hostRank)

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
	state := NewDeviceState(manager, cdi, cpManager, pulock, driverName)

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

	d := &driver{helper: helper, state: state, manager: manager}

	if err := d.publishResourceSlice(ctx); err != nil {
		return nil, fmt.Errorf("publish resource slice: %w", err)
	}

	return d, nil
}

func (d *driver) publishResourceSlice(ctx context.Context) error {
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

	resources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			m.nodeName: {
				Slices: []resourceslice.Slice{
					{Devices: []resourceapi.Device{device}},
				},
			},
		},
	}

	if err := d.helper.PublishResources(ctx, resources); err != nil {
		return err
	}
	klog.Infof("published ResourceSlice: device wormhole-t3k with %d chips on pool %s",
		m.chipCount, m.nodeName)
	return nil
}

func (d *driver) Stop() {
	d.helper.Stop()
}

func ptr[T any](v T) *T { return &v }
