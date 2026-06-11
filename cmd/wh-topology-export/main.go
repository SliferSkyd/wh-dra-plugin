// wh-topology-export dumps the *complete* physical topology a fabric-manager
// agent reports for this host — all four fields, including the three the DRA
// device-enumeration path does not need:
//
//	asics[]                 (per-chip inventory)
//	local_connections[]     (intra-host ethernet mesh)
//	exit_nodes[]            (cross-host links)
//	cluster_descriptor_yaml (UMD cluster descriptor consumed by tt-metal)
//
// It calls AgentService.GetTopology once and writes:
//
//	<out>.json                      asics + local_connections + exit_nodes + summary
//	<out>.cluster_descriptor.yaml   the raw cluster descriptor (only when present)
//
// The mesh fields and the cluster descriptor are populated only when the agent
// completed a UMD-based discovery. While the chips are busy the agent falls
// back to PCIe-only enumeration and those three come back empty — run on idle
// hardware, or pass -rediscover to force a fresh discovery first.
//
// Usage:
//
//	wh-topology-export [-addr host:port] [-out PREFIX] [-rediscover] [-stdout]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	agentpb "github.com/tenstorrent/wh-dra-plugin/internal/fabricmanager/proto/agent"
	topologypb "github.com/tenstorrent/wh-dra-plugin/internal/fabricmanager/proto/topology"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Export is the JSON document. Field names mirror the proto so the output
// reads the same as the topology.proto schema. cluster_descriptor_yaml is
// reported here as a byte count and written verbatim to its own file because
// it is multi-KB YAML.
type Export struct {
	Source                    string                 `json:"source"`
	Status                    string                 `json:"status"`
	AsicCount                 int                    `json:"asic_count"`
	LocalConnectionCount      int                    `json:"local_connection_count"`
	ExitNodeCount             int                    `json:"exit_node_count"`
	ClusterDescriptorYAMLSize int                    `json:"cluster_descriptor_yaml_bytes"`
	Asics                     []*topologypb.AsicInfo `json:"asics"`
	LocalConnections          []LocalConnection      `json:"local_connections"`
	ExitNodes                 []ExitNode             `json:"exit_nodes"`
}

type LocalConnection struct {
	SrcAsicID  uint64 `json:"src_asic_id"`
	SrcChannel uint32 `json:"src_channel"`
	DstAsicID  uint64 `json:"dst_asic_id"`
	DstChannel uint32 `json:"dst_channel"`
}

type ExitNode struct {
	LocalAsicID   uint64 `json:"local_asic_id"`
	LocalChannel  uint32 `json:"local_channel"`
	RemoteAsicID  uint64 `json:"remote_asic_id"`
	RemoteChannel uint32 `json:"remote_channel"`
}

func main() {
	var (
		addr       string
		out        string
		rediscover bool
		toStdout   bool
	)
	flag.StringVar(&addr, "addr", "localhost:50053", "fabric-manager agent address")
	flag.StringVar(&out, "out", "fm-topology", "output file prefix")
	flag.BoolVar(&rediscover, "rediscover", false, "force a fresh UMD discovery before exporting (fails on busy chips)")
	flag.BoolVar(&toStdout, "stdout", false, "also print the JSON document to stdout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("dial fabric manager agent at %q: %v", addr, err)
	}
	defer conn.Close()
	client := agentpb.NewAgentServiceClient(conn)

	topo, yaml, status, err := fetch(ctx, client, rediscover)
	if err != nil {
		fail("%v", err)
	}

	doc := Export{
		Source:                    addr,
		Status:                    status,
		AsicCount:                 len(topo.GetAsics()),
		LocalConnectionCount:      len(topo.GetLocalConnections()),
		ExitNodeCount:             len(topo.GetExitNodes()),
		ClusterDescriptorYAMLSize: len(yaml),
		Asics:                     topo.GetAsics(),
		LocalConnections:          make([]LocalConnection, 0, len(topo.GetLocalConnections())),
		ExitNodes:                 make([]ExitNode, 0, len(topo.GetExitNodes())),
	}
	for _, c := range topo.GetLocalConnections() {
		doc.LocalConnections = append(doc.LocalConnections, LocalConnection{
			SrcAsicID: c.GetSrcAsicId(), SrcChannel: c.GetSrcChannel(),
			DstAsicID: c.GetDstAsicId(), DstChannel: c.GetDstChannel(),
		})
	}
	for _, n := range topo.GetExitNodes() {
		doc.ExitNodes = append(doc.ExitNodes, ExitNode{
			LocalAsicID: n.GetLocalAsicId(), LocalChannel: n.GetLocalChannel(),
			RemoteAsicID: n.GetRemoteAsicId(), RemoteChannel: n.GetRemoteChannel(),
		})
	}

	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fail("marshal: %v", err)
	}
	b = append(b, '\n')

	jsonPath := out + ".json"
	if err := os.WriteFile(jsonPath, b, 0o644); err != nil {
		fail("write %s: %v", jsonPath, err)
	}

	yamlPath := ""
	if len(yaml) > 0 {
		yamlPath = out + ".cluster_descriptor.yaml"
		if err := os.WriteFile(yamlPath, []byte(yaml), 0o644); err != nil {
			fail("write %s: %v", yamlPath, err)
		}
	}

	if toStdout {
		os.Stdout.Write(b)
	}
	fmt.Printf("status=%s asics=%d local_connections=%d exit_nodes=%d cluster_descriptor_yaml=%dB\n",
		doc.Status, doc.AsicCount, doc.LocalConnectionCount, doc.ExitNodeCount, doc.ClusterDescriptorYAMLSize)
	fmt.Printf("wrote %s\n", jsonPath)
	if yamlPath != "" {
		fmt.Printf("wrote %s\n", yamlPath)
	} else {
		fmt.Println("cluster_descriptor_yaml empty (UMD discovery did not run — chips busy or fallback path); no YAML file written")
	}
}

// fetch returns the host topology, cluster descriptor YAML and a status
// string, either from the cached GetTopology state or via a forced
// RediscoverTopology when rediscover is set.
func fetch(ctx context.Context, client agentpb.AgentServiceClient, rediscover bool) (*topologypb.HostPhysicalTopology, string, string, error) {
	if rediscover {
		resp, err := client.RediscoverTopology(ctx, &agentpb.RediscoverTopologyRequest{})
		if err != nil {
			return nil, "", "", fmt.Errorf("RediscoverTopology: %w", err)
		}
		if msg := resp.GetMessage(); msg != "" {
			fmt.Fprintln(os.Stderr, "agent:", msg)
		}
		return resp.GetPhysicalTopology(), resp.GetClusterDescriptorYaml(), resp.GetStatus().String(), nil
	}
	resp, err := client.GetTopology(ctx, &agentpb.GetTopologyRequest{})
	if err != nil {
		return nil, "", "", fmt.Errorf("GetTopology: %w", err)
	}
	return resp.GetPhysicalTopology(), resp.GetClusterDescriptorYaml(), resp.GetStatus().String(), nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "wh-topology-export: "+format+"\n", args...)
	os.Exit(1)
}
