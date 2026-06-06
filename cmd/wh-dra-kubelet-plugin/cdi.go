package main

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

const (
	cdiVendor = "k8s.wormhole.tenstorrent.com"
	cdiClass  = "t3k"
	cdiKind   = cdiVendor + "/" + cdiClass
)

// cdiSpec is the top-level CDI spec file structure.
type cdiSpec struct {
	CDIVersion string      `json:"cdiVersion"`
	Kind       string      `json:"kind"`
	Devices    []cdiDevice `json:"devices"`
}

type cdiDevice struct {
	Name           string         `json:"name"`
	ContainerEdits cdiEdits       `json:"containerEdits"`
}

type cdiEdits struct {
	Env         []string    `json:"env,omitempty"`
	DeviceNodes []cdiDev    `json:"deviceNodes,omitempty"`
	Mounts      []cdiMount  `json:"mounts,omitempty"`
}

type cdiDev struct {
	Path string `json:"path"`
}

type cdiMount struct {
	HostPath      string   `json:"hostPath"`
	ContainerPath string   `json:"containerPath"`
	Options       []string `json:"options,omitempty"`
}

// CDIHandler manages CDI spec files for this node.
type CDIHandler struct {
	dir     string
	manager *WHManager
}

func NewCDIHandler(cdiDir string, manager *WHManager) (*CDIHandler, error) {
	if err := os.MkdirAll(cdiDir, 0750); err != nil {
		return nil, fmt.Errorf("create CDI dir: %w", err)
	}
	return &CDIHandler{dir: cdiDir, manager: manager}, nil
}

// CreateCommonSpecFile writes the node-level CDI spec (env vars + mounts).
// Called once at plugin startup.
func (h *CDIHandler) CreateCommonSpecFile() error {
	spec := cdiSpec{
		CDIVersion: "0.5.0",
		Kind:       cdiKind,
		Devices: []cdiDevice{
			{
				Name: "common",
				ContainerEdits: cdiEdits{
					Env: h.manager.CommonEnvs(),
					Mounts: []cdiMount{
						{
							HostPath:      "/tmp/tt_logs",
							ContainerPath: "/tmp/tt_logs",
							Options:       []string{"rw", "bind"},
						},
					},
				},
			},
		},
	}
	return h.writeSpec("common", spec)
}

// CreateClaimSpecFile writes a per-claim CDI spec with device nodes.
// Called in PrepareResourceClaims.
func (h *CDIHandler) CreateClaimSpecFile(claimUID string) error {
	var devNodes []cdiDev
	for _, dev := range h.manager.deviceNodes {
		devNodes = append(devNodes, cdiDev{Path: dev})
	}

	spec := cdiSpec{
		CDIVersion: "0.5.0",
		Kind:       cdiKind,
		Devices: []cdiDevice{
			{
				Name: claimUID,
				ContainerEdits: cdiEdits{
					DeviceNodes: devNodes,
					Env: []string{
						fmt.Sprintf("WH_RESOURCE_CLAIM_UID=%s", claimUID),
					},
				},
			},
		},
	}
	return h.writeSpec(claimUID, spec)
}

// DeleteClaimSpecFile removes a per-claim CDI spec.
func (h *CDIHandler) DeleteClaimSpecFile(claimUID string) error {
	path := h.specPath(claimUID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete CDI spec %s: %w", path, err)
	}
	return nil
}

// GetClaimDeviceIDs returns the CDI device IDs to pass back to kubelet.
func (h *CDIHandler) GetClaimDeviceIDs(claimUID string) []string {
	return []string{
		fmt.Sprintf("%s=common", cdiKind),
		fmt.Sprintf("%s=%s", cdiKind, claimUID),
	}
}

func (h *CDIHandler) writeSpec(name string, spec cdiSpec) error {
	b, err := yaml.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal CDI spec: %w", err)
	}
	path := h.specPath(name)
	if err := os.WriteFile(path, b, 0644); err != nil {
		return fmt.Errorf("write CDI spec %s: %w", path, err)
	}
	return nil
}

func (h *CDIHandler) specPath(name string) string {
	return filepath.Join(h.dir, fmt.Sprintf("%s-%s-%s.yaml", cdiVendor, cdiClass, name))
}
