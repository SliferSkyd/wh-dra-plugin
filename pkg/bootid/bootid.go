package bootid

import (
	"os"
	"strings"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// GetCurrentBootID returns the kernel boot UUID. Changes on every reboot.
// Used to detect stale checkpoints after a node restart.
func GetCurrentBootID() (string, error) {
	b, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
