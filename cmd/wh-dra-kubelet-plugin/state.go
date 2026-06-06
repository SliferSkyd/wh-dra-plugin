package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"

	flockpkg "github.com/tenstorrent/wh-dra-plugin/pkg/flock"
	"github.com/tenstorrent/wh-dra-plugin/pkg/metrics"
)

// DeviceState implements kubeletplugin.DRAPlugin.
type DeviceState struct {
	mu         sync.Mutex
	cdi        *CDIHandler
	manager    *WHManager
	cpManager  *CheckpointManager
	pulock     *flockpkg.Flock
	driverName string
	nodeName   string
}

func NewDeviceState(
	manager *WHManager,
	cdi *CDIHandler,
	cpManager *CheckpointManager,
	pulock *flockpkg.Flock,
	driverName string,
) *DeviceState {
	return &DeviceState{
		cdi:        cdi,
		manager:    manager,
		cpManager:  cpManager,
		pulock:     pulock,
		driverName: driverName,
		nodeName:   manager.nodeName,
	}
}

func (s *DeviceState) HandleError(ctx context.Context, err error, msg string) {
	klog.ErrorS(err, msg)
}

func (s *DeviceState) PrepareResourceClaims(
	ctx context.Context,
	claims []*resourceapi.ResourceClaim,
) (map[types.UID]kubeletplugin.PrepareResult, error) {
	done := metrics.TrackInFlight(s.driverName, "prepare")
	defer done()
	t0 := time.Now()

	release, err := s.pulock.Acquire(ctx)
	if err != nil {
		metrics.IncPrepareError(s.driverName, "lock_acquire")
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	defer release()

	s.mu.Lock()
	defer s.mu.Unlock()

	cp, err := s.cpManager.Load()
	if err != nil {
		metrics.IncPrepareError(s.driverName, "checkpoint_load")
		return nil, err
	}

	results := make(map[types.UID]kubeletplugin.PrepareResult, len(claims))
	for _, claim := range claims {
		uid := claim.UID

		// Idempotent: already prepared.
		if pc, ok := cp.PreparedClaims[string(uid)]; ok && pc.State == statePrepareCompleted {
			klog.V(4).Infof("claim %s already prepared, returning cached CDI IDs", uid)
			results[uid] = kubeletplugin.PrepareResult{
				Devices: []kubeletplugin.Device{{CDIDeviceIDs: pc.CDIDeviceIDs}},
			}
			continue
		}

		// Mark started before touching CDI files.
		cp.PreparedClaims[string(uid)] = &preparedClaim{State: statePrepareStarted}
		if err := s.cpManager.Save(cp); err != nil {
			metrics.IncPrepareError(s.driverName, "checkpoint_save")
			results[uid] = kubeletplugin.PrepareResult{Err: err}
			continue
		}

		if err := s.cdi.CreateClaimSpecFile(string(uid)); err != nil {
			metrics.IncPrepareError(s.driverName, "cdi_write")
			results[uid] = kubeletplugin.PrepareResult{Err: err}
			continue
		}

		cdiIDs := s.cdi.GetClaimDeviceIDs(string(uid))
		cp.PreparedClaims[string(uid)] = &preparedClaim{
			State:        statePrepareCompleted,
			CDIDeviceIDs: cdiIDs,
		}
		if err := s.cpManager.Save(cp); err != nil {
			metrics.IncPrepareError(s.driverName, "checkpoint_save")
			results[uid] = kubeletplugin.PrepareResult{Err: err}
			continue
		}

		results[uid] = kubeletplugin.PrepareResult{
			Devices: []kubeletplugin.Device{{CDIDeviceIDs: cdiIDs}},
		}
		klog.Infof("prepared claim %s → %v", uid, cdiIDs)
	}

	metrics.SetPreparedDevices(s.driverName, s.nodeName, len(cp.PreparedClaims))
	metrics.ObserveRequest(s.driverName, "prepare", time.Since(t0))
	return results, nil
}

func (s *DeviceState) UnprepareResourceClaims(
	ctx context.Context,
	claims []kubeletplugin.NamespacedObject,
) (map[types.UID]error, error) {
	done := metrics.TrackInFlight(s.driverName, "unprepare")
	defer done()
	t0 := time.Now()

	release, err := s.pulock.Acquire(ctx)
	if err != nil {
		metrics.IncUnprepareError(s.driverName, "lock_acquire")
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	defer release()

	s.mu.Lock()
	defer s.mu.Unlock()

	cp, err := s.cpManager.Load()
	if err != nil {
		metrics.IncUnprepareError(s.driverName, "checkpoint_load")
		return nil, err
	}

	results := make(map[types.UID]error, len(claims))
	for _, claim := range claims {
		uid := claim.UID
		if err := s.cdi.DeleteClaimSpecFile(string(uid)); err != nil {
			metrics.IncUnprepareError(s.driverName, "cdi_delete")
			results[uid] = err
			continue
		}
		delete(cp.PreparedClaims, string(uid))
		results[uid] = nil // explicit nil so kubelet marks claim as fully unprepared
		klog.Infof("unprepared claim %s", uid)
	}

	if err := s.cpManager.Save(cp); err != nil {
		metrics.IncUnprepareError(s.driverName, "checkpoint_save")
	}

	metrics.SetPreparedDevices(s.driverName, s.nodeName, len(cp.PreparedClaims))
	metrics.ObserveRequest(s.driverName, "unprepare", time.Since(t0))
	return results, nil
}
