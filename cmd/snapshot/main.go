/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// snapshot captures Kubernetes cluster state for offline simulation.
// It only requires kubeconfig access - no AWS credentials needed.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/simulation"

	// Import for side-effect: registers types to scheme.Scheme
	_ "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
)

var (
	outputFile  string
	clusterName string
	namespaces  string
	excludeNS   string
)

func init() {
	flag.StringVar(&outputFile, "output", "snapshot.yaml", "output file path")
	flag.StringVar(&clusterName, "cluster-name", "", "cluster name (required)")
	flag.StringVar(&namespaces, "namespaces", "", "namespaces to include (comma-separated, empty = all)")
	flag.StringVar(&excludeNS, "exclude-namespaces", "kube-system,kube-public,kube-node-lease", "namespaces to exclude (comma-separated)")
	flag.Parse()
}

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if clusterName == "" {
		slog.Error("cluster-name is required")
		flag.Usage()
		os.Exit(1)
	}

	slog.Info("capturing cluster snapshot", "cluster", clusterName, "output", outputFile)

	snapshot, err := captureSnapshot(ctx, clusterName)
	if err != nil {
		slog.Error("failed to capture snapshot", "error", err)
		os.Exit(1)
	}

	if err := simulation.SaveSnapshot(snapshot, outputFile); err != nil {
		slog.Error("failed to save snapshot", "error", err)
		os.Exit(1)
	}

	slog.Info("snapshot saved successfully",
		"output", outputFile,
		"nodes", len(snapshot.Nodes),
		"pods", len(snapshot.Pods),
		"nodePools", len(snapshot.NodePools),
		"ec2NodeClasses", len(snapshot.EC2NodeClasses),
		"nodeClaims", len(snapshot.NodeClaims),
		"priorityClasses", len(snapshot.PriorityClasses),
	)
}

func captureSnapshot(ctx context.Context, cluster string) (*simulation.ClusterSnapshot, error) {
	// Load kubeconfig
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	rawConfig, err := kubeConfig.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("getting raw kubeconfig: %w", err)
	}

	// Create kubernetes client using the standard scheme
	// Types are registered via init() functions in imported packages
	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	// Initialize snapshot (region is empty - not needed for k8s-only snapshot)
	snapshot := simulation.NewClusterSnapshot(cluster, "")
	snapshot.Metadata.Context = rawConfig.CurrentContext
	snapshot.Metadata.CapturedAt = time.Now()

	// Capture NodeClaims first (we need these to filter nodes)
	nodeClaimList := &karpv1.NodeClaimList{}
	if err := kubeClient.List(ctx, nodeClaimList); err != nil {
		return nil, fmt.Errorf("listing nodeclaims: %w", err)
	}

	nodeClaimsByNodeName := make(map[string]*karpv1.NodeClaim)
	for i := range nodeClaimList.Items {
		nc := &nodeClaimList.Items[i]
		if nc.Status.NodeName != "" {
			nodeClaimsByNodeName[nc.Status.NodeName] = nc
		}
		snapshot.NodeClaims = append(snapshot.NodeClaims, *nc)
	}
	slog.Info("captured nodeclaims", "count", len(snapshot.NodeClaims))

	// Capture Karpenter-managed nodes only
	nodeList := &corev1.NodeList{}
	if err := kubeClient.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	for _, node := range nodeList.Items {
		// Only include nodes that have a corresponding NodeClaim
		if _, ok := nodeClaimsByNodeName[node.Name]; !ok {
			continue
		}

		snapshot.Nodes = append(snapshot.Nodes, simulation.NodeSnapshot{
			Name:        node.Name,
			Labels:      node.Labels,
			ProviderID:  node.Spec.ProviderID,
			Taints:      node.Spec.Taints,
			Allocatable: node.Status.Allocatable,
		})
	}
	slog.Info("captured nodes", "count", len(snapshot.Nodes))

	// Capture pods
	podList := &corev1.PodList{}
	if err := kubeClient.List(ctx, podList); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	excludeNamespaces := parseNamespaceList(excludeNS)
	includeNamespaces := parseNamespaceList(namespaces)

	for _, pod := range podList.Items {
		// Skip excluded namespaces
		if _, excluded := excludeNamespaces[pod.Namespace]; excluded {
			continue
		}
		// If include list is specified, only include those
		if len(includeNamespaces) > 0 {
			if _, included := includeNamespaces[pod.Namespace]; !included {
				continue
			}
		}
		// Skip completed/failed pods
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		podSnapshot := convertPod(&pod)
		snapshot.Pods = append(snapshot.Pods, podSnapshot)
	}
	slog.Info("captured pods", "count", len(snapshot.Pods))

	// Capture NodePools
	nodePoolList := &karpv1.NodePoolList{}
	if err := kubeClient.List(ctx, nodePoolList); err != nil {
		return nil, fmt.Errorf("listing nodepools: %w", err)
	}
	snapshot.NodePools = nodePoolList.Items
	slog.Info("captured nodepools", "count", len(snapshot.NodePools))

	// Capture EC2NodeClasses
	ec2NodeClassList := &v1.EC2NodeClassList{}
	if err := kubeClient.List(ctx, ec2NodeClassList); err != nil {
		return nil, fmt.Errorf("listing ec2nodeclasses: %w", err)
	}
	snapshot.EC2NodeClasses = ec2NodeClassList.Items
	slog.Info("captured ec2nodeclasses", "count", len(snapshot.EC2NodeClasses))

	// Capture PriorityClasses
	priorityClassList := &schedulingv1.PriorityClassList{}
	if err := kubeClient.List(ctx, priorityClassList); err != nil {
		return nil, fmt.Errorf("listing priorityclasses: %w", err)
	}
	// Filter out system priority classes that start with "system-"
	for _, pc := range priorityClassList.Items {
		if !strings.HasPrefix(pc.Name, "system-") {
			snapshot.PriorityClasses = append(snapshot.PriorityClasses, pc)
		}
	}
	slog.Info("captured priorityclasses", "count", len(snapshot.PriorityClasses))

	return snapshot, nil
}

func convertPod(pod *corev1.Pod) simulation.PodSnapshot {
	return simulation.PodSnapshot{
		Metadata: simulation.PodMetadataSnapshot{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			Labels:          pod.Labels,
			Annotations:     filterKarpenterAnnotations(pod.Annotations),
			OwnerReferences: pod.OwnerReferences,
		},
		Spec: simulation.PodSpecSnapshot{
			NodeName:                  pod.Spec.NodeName,
			Containers:                convertContainers(pod.Spec.Containers),
			InitContainers:            convertContainers(pod.Spec.InitContainers),
			NodeSelector:              pod.Spec.NodeSelector,
			Affinity:                  pod.Spec.Affinity,
			TopologySpreadConstraints: pod.Spec.TopologySpreadConstraints,
			Tolerations:               pod.Spec.Tolerations,
			PriorityClassName:         pod.Spec.PriorityClassName,
			Priority:                  pod.Spec.Priority,
			RuntimeClassName:          pod.Spec.RuntimeClassName,
		},
		Status: simulation.PodStatusSnapshot{
			Phase:      pod.Status.Phase,
			Conditions: filterSchedulingConditions(pod.Status.Conditions),
		},
	}
}

func convertContainers(containers []corev1.Container) []simulation.ContainerSnapshot {
	result := make([]simulation.ContainerSnapshot, len(containers))
	for i, c := range containers {
		result[i] = simulation.ContainerSnapshot{
			Name:      c.Name,
			Resources: c.Resources,
		}
	}
	return result
}

func filterKarpenterAnnotations(annotations map[string]string) map[string]string {
	if annotations == nil {
		return nil
	}
	result := make(map[string]string)
	for k, v := range annotations {
		if strings.HasPrefix(k, "karpenter.sh/") {
			result[k] = v
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func filterSchedulingConditions(conditions []corev1.PodCondition) []corev1.PodCondition {
	// Only keep scheduling-related conditions
	return lo.Filter(conditions, func(c corev1.PodCondition, _ int) bool {
		return c.Type == corev1.PodScheduled || c.Type == corev1.PodReady
	})
}

func parseNamespaceList(s string) map[string]struct{} {
	result := make(map[string]struct{})
	if s == "" {
		return result
	}
	for _, ns := range strings.Split(s, ",") {
		ns = strings.TrimSpace(ns)
		if ns != "" {
			result[ns] = struct{}{}
		}
	}
	return result
}

// Ensure we have the right types registered
var _ metav1.Object = &corev1.Pod{}
