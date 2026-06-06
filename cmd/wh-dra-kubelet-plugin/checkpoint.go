package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tenstorrent/wh-dra-plugin/pkg/bootid"
)

type claimState string

const (
	statePrepareStarted   claimState = "PrepareStarted"
	statePrepareCompleted claimState = "PrepareCompleted"
)

type preparedClaim struct {
	State        claimState `json:"state"`
	CDIDeviceIDs []string   `json:"cdiDeviceIDs,omitempty"`
}

type checkpoint struct {
	NodeBootID     string                    `json:"nodeBootID"`
	PreparedClaims map[string]*preparedClaim `json:"preparedClaims,omitempty"`
	Checksum       string                    `json:"checksum,omitempty"`
}

func newCheckpoint() (*checkpoint, error) {
	bid, err := bootid.GetCurrentBootID()
	if err != nil {
		return nil, fmt.Errorf("get boot ID: %w", err)
	}
	return &checkpoint{
		NodeBootID:     bid,
		PreparedClaims: make(map[string]*preparedClaim),
	}, nil
}

func (c *checkpoint) computeChecksum() string {
	tmp := *c
	tmp.Checksum = ""
	b, _ := json.Marshal(tmp)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func (c *checkpoint) save(path string) error {
	c.Checksum = c.computeChecksum()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadOrNewCheckpoint(path string) (*checkpoint, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return newCheckpoint()
	}
	if err != nil {
		return nil, fmt.Errorf("read checkpoint: %w", err)
	}

	var cp checkpoint
	if err := json.Unmarshal(b, &cp); err != nil {
		return newCheckpoint() // corrupt file → start fresh
	}

	// Verify checksum.
	if cp.Checksum != cp.computeChecksum() {
		return newCheckpoint()
	}

	// Verify boot ID — node reboot clears /var/run/cdi/ but leaves this file.
	currentBID, err := bootid.GetCurrentBootID()
	if err != nil {
		return nil, err
	}
	if cp.NodeBootID != currentBID {
		return newCheckpoint() // stale after reboot
	}

	return &cp, nil
}

// CheckpointManager wraps checkpoint load/save with a fixed path.
type CheckpointManager struct {
	path string
}

func NewCheckpointManager(dir string) (*CheckpointManager, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, err
	}
	return &CheckpointManager{path: filepath.Join(dir, "checkpoint.json")}, nil
}

func (cm *CheckpointManager) Load() (*checkpoint, error) {
	return loadOrNewCheckpoint(cm.path)
}

func (cm *CheckpointManager) Save(cp *checkpoint) error {
	return cp.save(cm.path)
}
