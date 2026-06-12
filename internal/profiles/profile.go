// Package profiles defines the interface that hardware-specific device
// profiles must satisfy. A profile is responsible for discovering devices
// (via whatever mechanism fits the hardware — fabric manager gRPC, sysfs,
// etc.) and for answering CDI questions about which host device paths
// correspond to an allocated device.
package profiles

import (
	"context"

	"k8s.io/dynamic-resource-allocation/resourceslice"
)

// Profile is the interface for hardware-specific device enumeration and
// CDI injection decisions.
//
// Callers must treat the interface as goroutine-safe: EnumerateDevices may
// be called from the health-monitor goroutine while DeviceNodePaths is
// called from the kubelet PrepareResourceClaims handler.
type Profile interface {
	// EnumerateDevices discovers devices and returns the DriverResources
	// to publish in the ResourceSlice. The implementation is free to
	// contact external services (e.g. a fabric-manager agent) or read
	// sysfs. The result replaces the previously published slice when
	// PublishResources is called by the driver.
	EnumerateDevices(ctx context.Context) (resourceslice.DriverResources, error)

	// DeviceNodePaths returns the host /dev paths that must be injected
	// into a container for the named device. The device name matches the
	// Device.Name field published by EnumerateDevices. Returns an error
	// if the device is unknown or its paths cannot be determined.
	DeviceNodePaths(deviceName string) ([]string, error)
}
