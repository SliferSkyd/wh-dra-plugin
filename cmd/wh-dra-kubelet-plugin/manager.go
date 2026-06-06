package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	labelArch          = "tenstorrent.com/arch"
	labelBoardType     = "tenstorrent.com/board-type"
	labelChipCount     = "tenstorrent.com/chip-count"
	labelPhysicalPod   = "tenstorrent.com/physical-pod"
	labelHostRank      = "tenstorrent.com/host-rank"
	labelPodSize       = "tenstorrent.com/pod-size"
	labelEthernetIface = "tenstorrent.com/ethernet-iface"

	devTenstorrentDir = "/dev/tenstorrent"
)

// WHManager holds hardware facts derived from node labels and /dev/tenstorrent/.
type WHManager struct {
	nodeName      string
	arch          string
	boardType     string
	chipCount     int
	physicalPod   string
	hostRank      int
	podSize       int
	ethernetIface string
	deviceNodes   []string // e.g. ["/dev/tenstorrent/0", ..., "/dev/tenstorrent/3"]
}

func NewWHManager(ctx context.Context, nodeName string, k8s kubernetes.Interface) (*WHManager, error) {
	node, err := k8s.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %q: %w", nodeName, err)
	}

	m := &WHManager{nodeName: nodeName}
	if err := m.parseLabels(node); err != nil {
		return nil, err
	}
	if err := m.discoverDevices(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *WHManager) parseLabels(node *corev1.Node) error {
	labels := node.Labels

	required := func(key string) (string, error) {
		v, ok := labels[key]
		if !ok || v == "" {
			return "", fmt.Errorf("node label %q is required but missing", key)
		}
		return v, nil
	}
	requiredInt := func(key string) (int, error) {
		s, err := required(key)
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("label %q value %q is not an integer: %w", key, s, err)
		}
		return n, nil
	}

	var err error
	if m.arch, err = required(labelArch); err != nil {
		return err
	}
	if m.boardType, err = required(labelBoardType); err != nil {
		return err
	}
	if m.chipCount, err = requiredInt(labelChipCount); err != nil {
		return err
	}
	if m.physicalPod, err = required(labelPhysicalPod); err != nil {
		return err
	}
	if m.hostRank, err = requiredInt(labelHostRank); err != nil {
		return err
	}
	if m.podSize, err = requiredInt(labelPodSize); err != nil {
		return err
	}
	// ethernet-iface is optional (not all setups use it)
	m.ethernetIface = labels[labelEthernetIface]
	return nil
}

func (m *WHManager) discoverDevices() error {
	entries, err := os.ReadDir(devTenstorrentDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", devTenstorrentDir, err)
	}

	var nodes []string
	for _, e := range entries {
		if e.IsDir() {
			continue // skip by-id/
		}
		nodes = append(nodes, filepath.Join(devTenstorrentDir, e.Name()))
	}

	// Hard validation: label must match reality.
	if len(nodes) != m.chipCount {
		return fmt.Errorf(
			"chip-count label says %d but found %d devices in %s — check tt-kmd driver",
			m.chipCount, len(nodes), devTenstorrentDir,
		)
	}
	m.deviceNodes = nodes
	return nil
}

// CommonEnvs returns the env vars injected into every container on this node.
func (m *WHManager) CommonEnvs() []string {
	envs := []string{
		fmt.Sprintf("TT_MESH_HOST_RANK=%d", m.hostRank),
		fmt.Sprintf("TT_CHIP_COUNT=%d", m.chipCount),
		fmt.Sprintf("TT_POD_SIZE=%d", m.podSize),
		fmt.Sprintf("TT_PHYSICAL_POD=%s", m.physicalPod),
	}
	if m.ethernetIface != "" {
		envs = append(envs, fmt.Sprintf("TT_ETHERNET_IFACE=%s", m.ethernetIface))
	}
	return envs
}

// DeviceName returns the stable CDI device name for this node.
func (m *WHManager) DeviceName() string {
	return strings.ReplaceAll(m.nodeName, ".", "-")
}
