package main

import (
	"strings"
	"testing"
)

// envToMap splits "KEY=value" strings into a map for easy assertion.
func envToMap(envs []string) map[string]string {
	m := make(map[string]string)
	for _, e := range envs {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

func TestPoolName(t *testing.T) {
	tests := []struct {
		desc        string
		nodeName    string
		physicalPod string
		podSize     int
		want        string
	}{
		{
			desc:        "single-node uses nodeName",
			nodeName:    "worker-1",
			physicalPod: "t3k-pod-0",
			podSize:     1,
			want:        "worker-1",
		},
		{
			desc:        "2-node chip-to-chip uses physicalPod",
			nodeName:    "worker-1",
			physicalPod: "t3k-pod-0",
			podSize:     2,
			want:        "t3k-pod-0",
		},
		{
			desc:        "8-node Galaxy pod uses physicalPod",
			nodeName:    "galaxy-node-3",
			physicalPod: "galaxy-pod-0",
			podSize:     8,
			want:        "galaxy-pod-0",
		},
		{
			desc:        "both nodes in same pod return identical pool name",
			nodeName:    "worker-2",
			physicalPod: "t3k-pod-0",
			podSize:     2,
			want:        "t3k-pod-0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			m := &WHManager{
				nodeName:    tt.nodeName,
				physicalPod: tt.physicalPod,
				podSize:     tt.podSize,
			}
			if got := m.PoolName(); got != tt.want {
				t.Errorf("PoolName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCommonEnvs_VarNames(t *testing.T) {
	m := &WHManager{
		hostRank:    0,
		chipCount:   8,
		podSize:     1,
		physicalPod: "t3k-pod-0",
	}
	got := envToMap(m.CommonEnvs())

	// tt-metal reads TT_HOST_RANK, not TT_MESH_HOST_RANK.
	if _, bad := got["TT_MESH_HOST_RANK"]; bad {
		t.Error("TT_MESH_HOST_RANK must not appear in CommonEnvs; use TT_HOST_RANK")
	}
	if _, ok := got["TT_HOST_RANK"]; !ok {
		t.Error("TT_HOST_RANK must be present in CommonEnvs")
	}
	if _, ok := got["TT_MESH_ID"]; !ok {
		t.Error("TT_MESH_ID must be present in CommonEnvs")
	}
	if _, ok := got["TT_METAL_CACHE"]; !ok {
		t.Error("TT_METAL_CACHE must be present in CommonEnvs")
	}
}

func TestCommonEnvs_Values(t *testing.T) {
	tests := []struct {
		desc string
		m    *WHManager
		want map[string]string
	}{
		{
			desc: "rank-0 single-node",
			m: &WHManager{
				hostRank: 0, chipCount: 8, podSize: 1,
				physicalPod: "t3k-pod-0",
			},
			want: map[string]string{
				"TT_HOST_RANK":    "0",
				"TT_MESH_ID":      "0",
				"TT_METAL_CACHE":  "/tmp/tt-metal-cache-0",
				"TT_CHIP_COUNT":   "8",
				"TT_POD_SIZE":     "1",
				"TT_PHYSICAL_POD": "t3k-pod-0",
			},
		},
		{
			desc: "rank-1 two-node pod",
			m: &WHManager{
				hostRank: 1, chipCount: 8, podSize: 2,
				physicalPod: "t3k-pod-0",
			},
			want: map[string]string{
				"TT_HOST_RANK":    "1",
				"TT_MESH_ID":      "0",
				"TT_METAL_CACHE":  "/tmp/tt-metal-cache-1",
				"TT_CHIP_COUNT":   "8",
				"TT_POD_SIZE":     "2",
				"TT_PHYSICAL_POD": "t3k-pod-0",
			},
		},
		{
			desc: "rank-3 eight-node Galaxy",
			m: &WHManager{
				hostRank: 3, chipCount: 32, podSize: 8,
				physicalPod: "galaxy-pod-0",
			},
			want: map[string]string{
				"TT_HOST_RANK":   "3",
				"TT_METAL_CACHE": "/tmp/tt-metal-cache-3",
				"TT_CHIP_COUNT":  "32",
				"TT_POD_SIZE":    "8",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			got := envToMap(tt.m.CommonEnvs())
			for k, wantV := range tt.want {
				if got[k] != wantV {
					t.Errorf("env %s = %q, want %q", k, got[k], wantV)
				}
			}
		})
	}
}

func TestCommonEnvs_EthernetIface(t *testing.T) {
	t.Run("omitted when empty", func(t *testing.T) {
		m := &WHManager{physicalPod: "t3k-pod-0", podSize: 1, nodeName: "n1"}
		got := envToMap(m.CommonEnvs())
		if _, ok := got["TT_ETHERNET_IFACE"]; ok {
			t.Error("TT_ETHERNET_IFACE must not appear when ethernetIface is empty")
		}
	})
	t.Run("present when set", func(t *testing.T) {
		m := &WHManager{physicalPod: "t3k-pod-0", podSize: 1, nodeName: "n1", ethernetIface: "eth1"}
		got := envToMap(m.CommonEnvs())
		if got["TT_ETHERNET_IFACE"] != "eth1" {
			t.Errorf("TT_ETHERNET_IFACE = %q, want %q", got["TT_ETHERNET_IFACE"], "eth1")
		}
	})
}

// TestPoolName_BothNodesAgree verifies that two WHManagers representing the two
// hosts in a chip-to-chip pod return the same PoolName, which is the property
// that makes the scheduler treat them as a single logical device.
func TestPoolName_BothNodesAgree(t *testing.T) {
	nodeA := &WHManager{nodeName: "worker-1", physicalPod: "t3k-pod-0", podSize: 2}
	nodeB := &WHManager{nodeName: "worker-2", physicalPod: "t3k-pod-0", podSize: 2}
	if nodeA.PoolName() != nodeB.PoolName() {
		t.Errorf("pool names diverge: node-a %q != node-b %q", nodeA.PoolName(), nodeB.PoolName())
	}
}
