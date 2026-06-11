// wh-device-probe is a minimal, dependency-free enumerator for Tenstorrent
// chips on the local node. It reports exactly what the DRA kubelet plugin
// needs to publish a node's devices and build CDI edits: the chip count, the
// per-chip character device nodes, and the board arch/unique-id/PCI metadata.
//
// Unlike tt-smi or the fabric-manager agent, it reads only kernel-exported
// state (/dev/tenstorrent and /sys/class/tenstorrent) and never maps a chip's
// BAR0. It therefore works whether or not the chips are currently in use by a
// workload — the busy-device case where UMD-based discovery fails.
//
// Usage:
//
//	wh-device-probe              # human-readable summary
//	wh-device-probe -json        # machine-readable JSON
//	wh-device-probe -expect 32   # exit non-zero unless exactly 32 chips found
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	devDir   = "/dev/tenstorrent"
	byIDDir  = "/dev/tenstorrent/by-id"
	sysClass = "/sys/class/tenstorrent"
)

// Chip is the per-chip view the DRA plugin needs. Every field is sourced from
// the kernel without touching the device, so the struct is fully populated
// even while the chip is busy.
type Chip struct {
	DeviceNodeID   int    `json:"device_node_id"`             // the N in /dev/tenstorrent/N
	DeviceNodePath string `json:"device_node_path"`           // e.g. /dev/tenstorrent/0
	CharMajor      int64  `json:"char_major"`                 // cgroup device-rule major
	CharMinor      int64  `json:"char_minor"`                 // cgroup device-rule minor
	Arch           string `json:"arch,omitempty"`             // e.g. "wormhole" (from by-id)
	UniqueID       string `json:"unique_id,omitempty"`        // board unique id (from by-id)
	PCIAddress     string `json:"pci_address,omitempty"`      // BDF, e.g. 0000:00:10.0
	PCIID          string `json:"pci_id,omitempty"`           // vendor:device, e.g. 1E52:401E
}

// Report is the top-level JSON document emitted by -json.
type Report struct {
	ChipCount int    `json:"chip_count"`
	Chips     []Chip `json:"chips"`
}

func main() {
	var (
		asJSON bool
		expect int
	)
	flag.BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	flag.IntVar(&expect, "expect", 0, "if >0, exit non-zero unless exactly this many chips are found")
	flag.Parse()

	chips, err := enumerate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "wh-device-probe:", err)
		os.Exit(1)
	}

	report := Report{ChipCount: len(chips), Chips: chips}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printHuman(report)
	}

	if expect > 0 && len(chips) != expect {
		fmt.Fprintf(os.Stderr, "wh-device-probe: expected %d chips but found %d\n", expect, len(chips))
		os.Exit(2)
	}
}

// enumerate scans /dev/tenstorrent for character device nodes and enriches
// each with board metadata (from by-id symlinks) and PCI metadata (from
// sysfs). Chips are returned sorted by device node id for stable output.
func enumerate() ([]Chip, error) {
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", devDir, err)
	}

	byID := readByID() // device-node-id -> (arch, uniqueID); best-effort

	var chips []Chip
	for _, e := range entries {
		if e.IsDir() {
			continue // skip by-id/
		}
		id, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a numeric device node
		}
		path := filepath.Join(devDir, e.Name())

		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return nil, fmt.Errorf("stat %s: %w", path, err)
		}

		c := Chip{
			DeviceNodeID:   id,
			DeviceNodePath: path,
			CharMajor:      int64((st.Rdev >> 8) & 0xfff),
			CharMinor:      int64(st.Rdev & 0xff),
		}
		if bid, ok := byID[id]; ok {
			c.Arch = bid.arch
			c.UniqueID = bid.uniqueID
		}
		c.PCIAddress, c.PCIID = pciInfo(id)

		chips = append(chips, c)
	}

	sort.Slice(chips, func(i, j int) bool {
		return chips[i].DeviceNodeID < chips[j].DeviceNodeID
	})
	return chips, nil
}

type boardID struct{ arch, uniqueID string }

// readByID maps each device node id to its arch and unique id by resolving the
// /dev/tenstorrent/by-id/<arch>-<uniqueid> symlinks. Best-effort: a missing
// by-id directory yields an empty map and the caller proceeds without it.
func readByID() map[int]boardID {
	out := make(map[int]boardID)
	entries, err := os.ReadDir(byIDDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(byIDDir, e.Name()))
		if err != nil {
			continue
		}
		id, err := strconv.Atoi(filepath.Base(target)) // ../N -> N
		if err != nil {
			continue
		}
		arch, unique := "", e.Name()
		if i := strings.IndexByte(e.Name(), '-'); i >= 0 {
			arch, unique = e.Name()[:i], e.Name()[i+1:]
		}
		out[id] = boardID{arch: arch, uniqueID: unique}
	}
	return out
}

// pciInfo returns the PCI BDF and vendor:device id for a chip, read from
// sysfs. The class entry is named "tenstorrent!<N>" on disk. Both values are
// best-effort; empty strings are returned if sysfs is unavailable.
func pciInfo(id int) (bdf, pciID string) {
	base := filepath.Join(sysClass, fmt.Sprintf("tenstorrent!%d", id))

	if target, err := os.Readlink(filepath.Join(base, "device")); err == nil {
		bdf = filepath.Base(target) // .../0000:00:10.0 -> 0000:00:10.0
	}
	if data, err := os.ReadFile(filepath.Join(base, "device", "uevent")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PCI_ID="); ok {
				pciID = v
				break
			}
		}
	}
	return bdf, pciID
}

func printHuman(r Report) {
	fmt.Printf("Tenstorrent chips found: %d\n", r.ChipCount)
	for _, c := range r.Chips {
		fmt.Printf("  %s  (c %d:%d)  arch=%s  pci=%s  id=%s\n",
			c.DeviceNodePath, c.CharMajor, c.CharMinor,
			orNA(c.Arch), orNA(c.PCIAddress), orNA(c.UniqueID))
	}
}

func orNA(s string) string {
	if s == "" {
		return "n/a"
	}
	return s
}
