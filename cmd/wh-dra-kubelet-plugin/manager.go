package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

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

// DeviceNode holds the path and cgroup metadata for a single /dev/tenstorrent/N entry.
type DeviceNode struct {
	Path  string
	Type  string // "c" (character device)
	Major int64
	Minor int64
}

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
	deviceNodes   []DeviceNode // e.g. [{"/dev/tenstorrent/0", "c", 236, 0}, ...]
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

	var nodes []DeviceNode
	for _, e := range entries {
		if e.IsDir() {
			continue // skip by-id/
		}
		path := filepath.Join(devTenstorrentDir, e.Name())
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		devType := "c"
		if st.Mode&syscall.S_IFMT == syscall.S_IFBLK {
			devType = "b"
		}
		nodes = append(nodes, DeviceNode{
			Path:  path,
			Type:  devType,
			Major: int64((st.Rdev >> 8) & 0xfff),
			Minor: int64(st.Rdev & 0xff),
		})
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

// PoolName returns the ResourceSlice pool key for this node.
// When podSize > 1 (chip-to-chip multi-node), all hosts in the same physicalPod
// publish under a shared name so the scheduler groups them as one logical device.
func (m *WHManager) PoolName() string {
	if m.podSize > 1 {
		return m.physicalPod
	}
	return m.nodeName
}

// PoolTotalSliceCount returns the TotalSliceCount to set on the pool.
// For multi-node pools this equals podSize: the scheduler will not allocate
// from the pool until it can see all podSize slices (one per node).
// Zero for single-node pools — the library default (len(Slices)) is correct.
func (m *WHManager) PoolTotalSliceCount() int64 {
	if m.podSize > 1 {
		return int64(m.podSize)
	}
	return 0
}

// CommonEnvs returns the env vars injected into every container on this node.
func (m *WHManager) CommonEnvs() []string {
	envs := []string{
		fmt.Sprintf("TT_HOST_RANK=%d", m.hostRank),
		fmt.Sprintf("TT_MESH_ID=0"),
		fmt.Sprintf("TT_METAL_CACHE=/tmp/tt-metal-cache-%d", m.hostRank),
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
