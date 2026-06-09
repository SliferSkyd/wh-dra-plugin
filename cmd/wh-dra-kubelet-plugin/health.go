package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// ttSMISnapshot is the JSON structure returned by `tt-smi -s`.
type ttSMISnapshot struct {
	DeviceInfo []struct {
		Telemetry map[string]interface{} `json:"telemetry"`
		BoardInfo struct {
			BoardType string `json:"board_type"`
		} `json:"board_info"`
	} `json:"device_info"`
}

type healthChecker struct {
	ttSmiPath string
	interval  time.Duration
	chipCount int
	prevHB    []float64 // previous heartbeat value per chip
}

func newHealthChecker(ttSmiPath string, interval time.Duration, chipCount int) *healthChecker {
	return &healthChecker{
		ttSmiPath: ttSmiPath,
		interval:  interval,
		chipCount: chipCount,
		prevHB:    make([]float64, chipCount),
	}
}

// checkOnce runs tt-smi once and returns whether all chips are healthy.
func (h *healthChecker) checkOnce(ctx context.Context) (bool, string) {
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(runCtx, h.ttSmiPath, "-s")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	// tt-smi sometimes exits non-zero (driver warnings) but still writes valid
	// JSON to stdout. Try to parse whatever we got before declaring failure.
	if runErr != nil && len(out) == 0 {
		return false, fmt.Sprintf("tt-smi failed with no output: %v stderr=%q", runErr, stderr.String())
	}
	if runErr != nil {
		klog.V(4).Infof("tt-smi exited non-zero (%v) but produced output — attempting parse", runErr)
	}

	var snap ttSMISnapshot
	if err := json.Unmarshal(out, &snap); err != nil {
		return false, fmt.Sprintf("parse tt-smi output (runErr=%v): %v", runErr, err)
	}

	for i := 0; i < h.chipCount; i++ {
		if i >= len(snap.DeviceInfo) {
			return false, fmt.Sprintf("chip %d missing from tt-smi output (%d reported)", i, len(snap.DeviceInfo))
		}

		dev := snap.DeviceInfo[i]

		// Board info present → chip is at least visible
		if dev.BoardInfo.BoardType == "" {
			return false, fmt.Sprintf("chip %d: empty board info", i)
		}

		// Heartbeat check: must be present and strictly increasing
		hbRaw, ok := dev.Telemetry["heartbeat"]
		if !ok {
			continue // older firmware without heartbeat — assume healthy
		}
		hb, err := strconv.ParseFloat(fmt.Sprintf("%v", hbRaw), 64)
		if err != nil || hb <= 0 {
			return false, fmt.Sprintf("chip %d: invalid heartbeat %v", i, hbRaw)
		}
		if h.prevHB[i] > 0 && hb <= h.prevHB[i] {
			return false, fmt.Sprintf("chip %d: heartbeat stalled (prev=%.0f cur=%.0f)", i, h.prevHB[i], hb)
		}
		h.prevHB[i] = hb
	}

	return true, fmt.Sprintf("all %d chips healthy", h.chipCount)
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
