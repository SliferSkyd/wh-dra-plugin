package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
)

type config struct {
	nodeName        string
	pluginDir       string
	registrarDir    string
	cdiDir          string
	checkpointDir   string
	metricsPort     int
	ttSmiPath       string
	healthInterval  time.Duration
}

func main() {
	klog.InitFlags(nil)

	cfg := &config{}
	flag.StringVar(&cfg.nodeName, "node-name", os.Getenv("NODE_NAME"), "Kubernetes node name (env: NODE_NAME)")
	flag.StringVar(&cfg.pluginDir, "plugin-dir", "/var/lib/kubelet/plugins/wormhole.tenstorrent.com", "Kubelet plugin socket directory")
	flag.StringVar(&cfg.registrarDir, "registrar-dir", "/var/lib/kubelet/plugins_registry", "Kubelet plugin registrar directory")
	flag.StringVar(&cfg.cdiDir, "cdi-dir", "/var/run/cdi", "CDI spec directory")
	flag.StringVar(&cfg.checkpointDir, "checkpoint-dir", "/var/lib/wh-dra/checkpoint", "Checkpoint directory")
	flag.IntVar(&cfg.metricsPort, "metrics-port", 9090, "Prometheus metrics HTTP port")
	flag.StringVar(&cfg.ttSmiPath, "tt-smi-path", "", "Path to tt-smi binary; empty disables health monitoring")
	flag.DurationVar(&cfg.healthInterval, "health-check-interval", 30*time.Second, "How often to run tt-smi health checks (0 to disable)")
	var kubeconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "Path to kubeconfig (leave empty to use in-cluster config)")
	flag.Parse()

	if cfg.nodeName == "" {
		fmt.Fprintln(os.Stderr, "error: --node-name or NODE_NAME is required")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var k8sCfg *rest.Config
	var err error
	if kubeconfig != "" {
		k8sCfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	} else {
		k8sCfg, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatalf("k8s config: %v", err)
	}
	k8s, err := kubernetes.NewForConfig(k8sCfg)
	if err != nil {
		klog.Fatalf("k8s client: %v", err)
	}

	d, err := NewDriver(ctx, cfg, k8s)
	if err != nil {
		klog.Fatalf("init driver: %v", err)
	}
	defer d.Stop()

	d.startHealthMonitoring(ctx, cfg.ttSmiPath, cfg.healthInterval)

	// Prometheus metrics endpoint.
	go func() {
		addr := fmt.Sprintf(":%d", cfg.metricsPort)
		http.Handle("/metrics", promhttp.Handler())
		klog.Infof("metrics on %s/metrics", addr)
		if err := http.ListenAndServe(addr, nil); err != nil {
			klog.Errorf("metrics server: %v", err)
		}
	}()

	klog.Infof("wh-dra-kubelet-plugin running on node %s", cfg.nodeName)
	<-ctx.Done()
	klog.Info("shutting down")
}
