package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	topologyConfigMap = "tt-node-topology"
	topologyNamespace = "kube-system"
	devTenstorrent    = "/dev/tenstorrent"
)

func main() {
	klog.InitFlags(nil)

	var (
		nodeName  string
		ttSmiPath string
		interval  time.Duration
	)
	flag.StringVar(&nodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes node name (env: NODE_NAME)")
	flag.StringVar(&ttSmiPath, "tt-smi-path", "/usr/local/bin/tt-smi", "Path to tt-smi binary")
	flag.DurationVar(&interval, "interval", 5*time.Minute, "How often to re-apply labels")
	flag.Parse()

	if nodeName == "" {
		fmt.Fprintln(os.Stderr, "error: --node-name or NODE_NAME is required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	chips, err := countChips()
	if err != nil || chips == 0 {
		klog.Infof("no Tenstorrent chips found in %s — not a T3K node, sleeping", devTenstorrent)
		<-ctx.Done()
		return
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("k8s config: %v", err)
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("k8s client: %v", err)
	}

	l := &labeler{nodeName: nodeName, ttSmiPath: ttSmiPath, chips: chips, k8s: k8s}

	if err := l.apply(ctx); err != nil {
		klog.Errorf("apply labels: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := l.apply(ctx); err != nil {
				klog.Errorf("apply labels: %v", err)
			}
		}
	}
}

type labeler struct {
	nodeName  string
	ttSmiPath string
	chips     int
	k8s       kubernetes.Interface
}

func countChips() (int, error) {
	entries, err := os.ReadDir(devTenstorrent)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n, nil
}

func (l *labeler) apply(ctx context.Context) error {
	boardType, arch := l.discoverHardware()
	topology := l.readTopology(ctx)

	labels := map[string]string{
		"tenstorrent.com/arch":              arch,
		"tenstorrent.com/board-type":        boardType,
		"tenstorrent.com/chip-count":        strconv.Itoa(l.chips),
		"moai.moreh.io/accelerator.vendor":  "tenstorrent",
		"moai.moreh.io/accelerator.model":   arch,
	}
	for k, v := range topology {
		labels["tenstorrent.com/"+k] = v
	}

	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": labels},
	})
	if _, err := l.k8s.CoreV1().Nodes().Patch(ctx, l.nodeName,
		types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch node %s: %w", l.nodeName, err)
	}

	klog.Infof("labeled node %s: arch=%s board=%s chips=%d topology=%v",
		l.nodeName, arch, boardType, l.chips, topology)
	return nil
}

type ttSMIOutput struct {
	DeviceInfo []struct {
		BoardInfo struct {
			BoardType string `json:"board_type"`
		} `json:"board_info"`
	} `json:"device_info"`
}

func (l *labeler) discoverHardware() (boardType, arch string) {
	cmd := exec.Command(l.ttSmiPath, "-s")
	out, runErr := cmd.Output()
	// tt-smi sometimes exits non-zero (warnings/driver quirks) but still writes
	// valid JSON to stdout. Try to parse whatever we got before giving up.
	if runErr != nil && len(out) == 0 {
		klog.Warningf("tt-smi produced no output: %v — using defaults", runErr)
		return "unknown", "wormhole"
	}
	if runErr != nil {
		klog.V(4).Infof("tt-smi exited non-zero (%v) but produced output — attempting parse", runErr)
	}
	var snap ttSMIOutput
	if err := json.Unmarshal(out, &snap); err != nil || len(snap.DeviceInfo) == 0 {
		klog.Warningf("tt-smi JSON parse failed (runErr=%v parseErr=%v) — using defaults", runErr, err)
		return "unknown", "wormhole"
	}
	// board_type is e.g. "n300 L" or "n300 R" — take only the first word so the
	// label value is a valid Kubernetes identifier (no spaces).
	raw := strings.ToLower(strings.Fields(snap.DeviceInfo[0].BoardInfo.BoardType)[0])
	boardType = strings.TrimPrefix(raw, "wormhole_")
	if strings.HasPrefix(boardType, "p") {
		arch = "blackhole"
	} else {
		arch = "wormhole"
	}
	return boardType, arch
}

// readTopology reads per-node topology (physical-pod, host-rank, pod-size) from
// the tt-node-topology ConfigMap. Falls back to safe defaults if not found.
func (l *labeler) readTopology(ctx context.Context) map[string]string {
	defaults := map[string]string{
		"physical-pod": l.nodeName,
		"host-rank":    "0",
		"pod-size":     "1",
	}

	cm, err := l.k8s.CoreV1().ConfigMaps(topologyNamespace).Get(ctx, topologyConfigMap, metav1.GetOptions{})
	if err != nil {
		klog.V(4).Infof("ConfigMap %s not found, using defaults: %v", topologyConfigMap, err)
		return defaults
	}

	entry, ok := cm.Data[l.nodeName]
	if !ok {
		klog.V(4).Infof("no topology entry for %s in ConfigMap, using defaults", l.nodeName)
		return defaults
	}

	result := make(map[string]string)
	for _, pair := range strings.Fields(entry) {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}
