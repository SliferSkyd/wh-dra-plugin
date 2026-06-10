package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"k8s.io/klog/v2"
)

type healthChecker struct {
	chipCount int
	interval  time.Duration
}

func newHealthChecker(_ string, interval time.Duration, chipCount int) *healthChecker {
	return &healthChecker{chipCount: chipCount, interval: interval}
}

// checkOnce verifies every /dev/tenstorrent/N device node is accessible.
// We open the file rather than spawning tt-smi (a Python process that hangs
// when the chip ethernet mesh is in an inconsistent state after a reboot).
func (h *healthChecker) checkOnce(_ context.Context) (bool, string) {
	for i := 0; i < h.chipCount; i++ {
		path := fmt.Sprintf("/dev/tenstorrent/%d", i)
		f, err := os.Open(path)
		if err != nil {
			return false, fmt.Sprintf("chip %d not accessible (%s): %v", i, path, err)
		}
		f.Close()
	}
	return true, fmt.Sprintf("all %d chips accessible", h.chipCount)
}

// run polls health on the configured interval and calls onChange whenever
// the healthy/unhealthy status flips.
func (h *healthChecker) run(ctx context.Context, onChange func(healthy bool)) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	current := true // assume healthy at start

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			healthy, reason := h.checkOnce(ctx)
			klog.V(4).Infof("health check: healthy=%v %s", healthy, reason)
			if healthy != current {
				current = healthy
				klog.Infof("T3K health changed: healthy=%v — %s", healthy, reason)
				onChange(healthy)
			}
		}
	}
}
