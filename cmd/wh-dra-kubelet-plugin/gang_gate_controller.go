package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	gangGroupLabel   = "tenstorrent.com/gang-group"
	gangSizeLabel    = "tenstorrent.com/gang-size"
	gangGateName     = "tenstorrent.com/gang-ready"
	gangPollInterval = 2 * time.Second
)

// startGangGateController watches pods that carry the tenstorrent.com/gang-ready
// scheduling gate. When every expected pod in a gang group exists (count ==
// tenstorrent.com/gang-size) it removes the gate from all pods in the group
// simultaneously, allowing the default scheduler to place them in one pass.
//
// Multiple DaemonSet instances running this controller are safe: the remove-gate
// patch is idempotent (already-absent gate is a no-op), and the count check is
// monotonic — a partially released group stays >= expected, so straggler
// instances complete the remaining removals instead of re-blocking.
func startGangGateController(ctx context.Context, client kubernetes.Interface) {
	go func() {
		ticker := time.NewTicker(gangPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := reconcileGangGates(ctx, client); err != nil {
					klog.Errorf("gang gate reconcile: %v", err)
				}
			}
		}
	}()
}

type groupKey struct {
	namespace string
	group     string
}

type groupInfo struct {
	wantSize int
	total    []*corev1.Pod // all active pods with this gang-group label
	gated    []*corev1.Pod // subset that still have the scheduling gate
}

func reconcileGangGates(ctx context.Context, client kubernetes.Interface) error {
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		LabelSelector: gangGroupLabel,
	})
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	groups := make(map[groupKey]*groupInfo)

	for i := range pods.Items {
		pod := &pods.Items[i]

		// Skip terminating or completed pods — they don't count toward quorum.
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		group := pod.Labels[gangGroupLabel]
		if group == "" {
			continue
		}
		sizeStr := pod.Labels[gangSizeLabel]
		size, err := strconv.Atoi(sizeStr)
		if err != nil || size <= 0 {
			klog.Warningf("pod %s/%s: invalid %s=%q, skipping", pod.Namespace, pod.Name, gangSizeLabel, sizeStr)
			continue
		}

		key := groupKey{namespace: pod.Namespace, group: group}
		if groups[key] == nil {
			groups[key] = &groupInfo{wantSize: size}
		}
		info := groups[key]
		info.total = append(info.total, pod)
		if podHasGangGate(pod) {
			info.gated = append(info.gated, pod)
		}
	}

	for key, info := range groups {
		if len(info.gated) == 0 {
			continue // all gates already removed
		}
		if len(info.total) < info.wantSize {
			klog.V(4).Infof("gang %s/%s: %d/%d pods present, waiting",
				key.namespace, key.group, len(info.total), info.wantSize)
			continue
		}
		klog.Infof("gang %s/%s: all %d pods present, removing scheduling gate from %d pod(s)",
			key.namespace, key.group, info.wantSize, len(info.gated))
		for _, pod := range info.gated {
			if err := removeGangGate(ctx, client, pod); err != nil {
				klog.Errorf("remove gate %s from pod %s/%s: %v",
					gangGateName, pod.Namespace, pod.Name, err)
			}
		}
	}
	return nil
}

func podHasGangGate(pod *corev1.Pod) bool {
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name == gangGateName {
			return true
		}
	}
	return false
}

func removeGangGate(ctx context.Context, client kubernetes.Interface, pod *corev1.Pod) error {
	var remaining []corev1.PodSchedulingGate
	for _, g := range pod.Spec.SchedulingGates {
		if g.Name != gangGateName {
			remaining = append(remaining, g)
		}
	}
	// Empty slice serialises as [] which the API treats as "clear all gates".
	// nil would serialise as null, which is also accepted, but [] is unambiguous.
	if remaining == nil {
		remaining = []corev1.PodSchedulingGate{}
	}

	type specPatch struct {
		SchedulingGates []corev1.PodSchedulingGate `json:"schedulingGates"`
	}
	type podPatch struct {
		Spec specPatch `json:"spec"`
	}
	data, err := json.Marshal(podPatch{Spec: specPatch{SchedulingGates: remaining}})
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	_, err = client.CoreV1().Pods(pod.Namespace).Patch(
		ctx, pod.Name, types.MergePatchType, data, metav1.PatchOptions{},
	)
	return err
}
