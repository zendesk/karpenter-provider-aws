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

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"

	_ "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	awscloudprovider "github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/operator"
	"github.com/aws/karpenter-provider-aws/pkg/simulation"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
)

// SimulationMode determines what we're simulating
type SimulationMode string

const (
	ModePending     SimulationMode = "pending"      // Simulate scheduling pending pods
	ModeReschedule  SimulationMode = "reschedule"   // Simulate rescheduling pods from specific nodes
	ModeEfficiency  SimulationMode = "efficiency"   // Analyze current cluster efficiency
	ModeSynthetic   SimulationMode = "synthetic"    // Use synthetic workload (original behavior)
	ModeZoneFailure SimulationMode = "zone-failure" // Simulate zone/datacenter failure
)

// PodSummary captures essential pod information for simulation
type PodSummary struct {
	Name                      string
	Namespace                 string
	CPU                       resource.Quantity
	Memory                    resource.Quantity
	NodeSelector              map[string]string
	Tolerations               []corev1.Toleration
	Affinity                  *corev1.Affinity
	TopologySpreadConstraints []corev1.TopologySpreadConstraint
	OwnerKind                 string
	OwnerName                 string
	NodeName                  string // Current node (if scheduled)
}

// NodeSummary captures node information
type NodeSummary struct {
	Name           string
	InstanceType   string
	Zone           string
	CapacityType   string
	Allocatable    corev1.ResourceList
	AllocatedCPU   resource.Quantity
	AllocatedMem   resource.Quantity
	PodCount       int
	CPUUtilization float64
	MemUtilization float64
}

var (
	clusterName     string
	mode            string
	namespace       string
	labelSelector   string
	nodeSelector    string
	excludeFamilies string
	showTopN        int
	fromSnapshot    string
	zoneSelector    string

	// Synthetic mode options
	podCPU      string
	podMemory   string
	podReplicas int
)

func init() {
	flag.StringVar(&clusterName, "cluster-name", "", "cluster name (required for live mode)")
	flag.StringVar(&fromSnapshot, "from-snapshot", "", "path to snapshot file for offline simulation")
	flag.StringVar(&mode, "mode", "efficiency", "simulation mode: pending, reschedule, efficiency, synthetic, zone-failure")
	flag.StringVar(&namespace, "namespace", "", "namespace to filter pods (empty = all namespaces)")
	flag.StringVar(&labelSelector, "selector", "", "label selector for pods (e.g., app=myapp)")
	flag.StringVar(&nodeSelector, "node", "", "node name to analyze or reschedule from")
	flag.StringVar(&zoneSelector, "zone", "", "zone to simulate failure for (zone-failure mode)")
	flag.StringVar(&excludeFamilies, "exclude-families", "", "comma-separated instance families to exclude")
	flag.IntVar(&showTopN, "top", 10, "show top N results")

	// Synthetic mode
	flag.StringVar(&podCPU, "cpu", "500m", "CPU per pod (synthetic mode)")
	flag.StringVar(&podMemory, "memory", "512Mi", "Memory per pod (synthetic mode)")
	flag.IntVar(&podReplicas, "replicas", 100, "Number of replicas (synthetic mode)")

	flag.Parse()
}

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// If using snapshot mode, run offline simulation
	if fromSnapshot != "" {
		runSnapshotSimulation(ctx, fromSnapshot)
		return
	}

	// Live cluster mode requires cluster-name
	if clusterName == "" {
		slog.Error("either --cluster-name or --from-snapshot is required")
		flag.Usage()
		os.Exit(1)
	}

	slog.Info("starting simulation", "cluster", clusterName, "mode", mode)

	// Setup everything
	sim, err := NewSimulator(ctx, clusterName)
	if err != nil {
		slog.Error("failed to create simulator", "error", err)
		os.Exit(1)
	}

	switch SimulationMode(mode) {
	case ModeEfficiency:
		sim.RunEfficiencyAnalysis(ctx, namespace, labelSelector, nodeSelector)
	case ModePending:
		sim.RunPendingPodsAnalysis(ctx, namespace, labelSelector)
	case ModeReschedule:
		if nodeSelector == "" {
			slog.Error("--node is required for reschedule mode")
			os.Exit(1)
		}
		sim.RunRescheduleAnalysis(ctx, nodeSelector, excludeFamilies)
	case ModeSynthetic:
		sim.RunSyntheticAnalysis(ctx, podCPU, podMemory, podReplicas, excludeFamilies, showTopN)
	default:
		slog.Error("unknown mode", "mode", mode)
		os.Exit(1)
	}
}

// runSnapshotSimulation runs offline simulation from a snapshot file
func runSnapshotSimulation(ctx context.Context, snapshotPath string) {
	slog.Info("loading snapshot", "path", snapshotPath)

	snapshot, err := simulation.LoadSnapshot(snapshotPath)
	if err != nil {
		slog.Error("failed to load snapshot", "error", err)
		os.Exit(1)
	}

	if err := simulation.ValidateSnapshot(snapshot); err != nil {
		slog.Error("invalid snapshot", "error", err)
		os.Exit(1)
	}

	slog.Info("snapshot loaded",
		"cluster", snapshot.Metadata.ClusterName,
		"capturedAt", snapshot.Metadata.CapturedAt,
		"instanceTypes", len(snapshot.InstanceTypes),
		"nodes", len(snapshot.Nodes),
		"pods", len(snapshot.Pods),
	)

	sim := &SnapshotSimulator{snapshot: snapshot}

	switch SimulationMode(mode) {
	case ModeEfficiency:
		sim.RunEfficiencyAnalysis(namespace, labelSelector, nodeSelector)
	case ModePending:
		sim.RunPendingPodsAnalysis(namespace, labelSelector)
	case ModeReschedule:
		if nodeSelector == "" {
			slog.Error("--node is required for reschedule mode")
			os.Exit(1)
		}
		sim.RunRescheduleAnalysis(nodeSelector, excludeFamilies)
	case ModeSynthetic:
		sim.RunSyntheticAnalysis(podCPU, podMemory, podReplicas, excludeFamilies, showTopN)
	default:
		slog.Error("unknown mode", "mode", mode)
		os.Exit(1)
	}
}

// Simulator holds all the components needed for simulation
type Simulator struct {
	kubeClient    client.Client
	cloudProvider *awscloudprovider.CloudProvider
	cluster       *state.Cluster
	provisioner   *provisioning.Provisioner
	clk           clock.Clock
}

func NewSimulator(ctx context.Context, cluster string) (*Simulator, error) {
	// Disable leader election
	os.Setenv("DISABLE_LEADER_ELECTION", "true")

	// Set required args
	os.Args = append(os.Args, fmt.Sprintf("-cluster-name=%s", cluster))
	os.Args = append(os.Args, "-cluster-endpoint=https://kubernetes.default.svc.cluster.local/")

	// Load kubeconfig
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	// Create manager
	mgr, err := manager.New(restConfig, manager.Options{})
	if err != nil {
		return nil, fmt.Errorf("creating manager: %w", err)
	}

	// Create core operator
	coreOp := &coreoperator.Operator{
		Manager:             mgr,
		KubernetesInterface: kubernetes.NewForConfigOrDie(restConfig),
		Clock:               clock.RealClock{},
	}

	// Create AWS operator
	ctx, op := operator.NewOperator(ctx, coreOp)

	// Start cache in background
	go func() {
		_ = mgr.GetCache().Start(ctx)
	}()

	// Wait for cache sync
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		return nil, fmt.Errorf("cache sync failed")
	}

	clk := clock.RealClock{}
	recorder := events.NewRecorder(&record.FakeRecorder{})

	// Create cloud provider
	cp := awscloudprovider.New(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		recorder,
		op.GetClient(),
		op.AMIProvider,
		op.SecurityGroupProvider,
		op.CapacityReservationProvider,
		op.InstanceTypeStore,
	)

	// Wrap with metrics and overlay
	decoratedCP := overlay.Decorate(metrics.Decorate(cp), op.GetClient(), op.InstanceTypeStore)

	// Create cluster state
	clusterState := state.NewCluster(clk, op.GetClient(), decoratedCP)

	// Hydrate cluster state with current nodes
	nodeList := &corev1.NodeList{}
	if err := op.GetClient().List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	for _, node := range nodeList.Items {
		if err := clusterState.UpdateNode(ctx, &node); err != nil {
			slog.Warn("failed to update node in cluster state", "node", node.Name, "error", err)
		}
	}

	// Hydrate with nodeclaims
	nodeClaimList := &karpv1.NodeClaimList{}
	if err := op.GetClient().List(ctx, nodeClaimList); err != nil {
		return nil, fmt.Errorf("listing nodeclaims: %w", err)
	}
	for _, nc := range nodeClaimList.Items {
		clusterState.UpdateNodeClaim(&nc)
	}

	// Create provisioner
	provisioner := provisioning.NewProvisioner(
		op.GetClient(),
		recorder,
		cp,
		clusterState,
		clk,
	)

	slog.Info("simulator initialized",
		"nodes", len(nodeList.Items),
		"nodeclaims", len(nodeClaimList.Items),
	)

	return &Simulator{
		kubeClient:    op.GetClient(),
		cloudProvider: cp,
		cluster:       clusterState,
		provisioner:   provisioner,
		clk:           clk,
	}, nil
}

// RunEfficiencyAnalysis analyzes current cluster bin packing efficiency
func (s *Simulator) RunEfficiencyAnalysis(ctx context.Context, ns, labelSel, nodeName string) {
	slog.Info("running efficiency analysis")

	// Get nodes
	nodes, err := s.getNodes(ctx, nodeName)
	if err != nil {
		slog.Error("failed to get nodes", "error", err)
		return
	}

	// Get pods
	pods, err := s.getPods(ctx, ns, labelSel)
	if err != nil {
		slog.Error("failed to get pods", "error", err)
		return
	}

	// Build node summaries with pod allocations
	nodeSummaries := s.buildNodeSummaries(nodes, pods)

	// Print analysis
	s.printEfficiencyReport(nodeSummaries, pods)
}

// RunPendingPodsAnalysis shows what Karpenter would provision for pending pods
func (s *Simulator) RunPendingPodsAnalysis(ctx context.Context, ns, labelSel string) {
	slog.Info("analyzing pending pods")

	// Get pending pods
	pods, err := s.getPods(ctx, ns, labelSel)
	if err != nil {
		slog.Error("failed to get pods", "error", err)
		return
	}

	pendingPods := lo.Filter(pods, func(p corev1.Pod, _ int) bool {
		return p.Status.Phase == corev1.PodPending && p.Spec.NodeName == ""
	})

	if len(pendingPods) == 0 {
		slog.Info("no pending pods found")
		return
	}

	slog.Info("found pending pods", "count", len(pendingPods))

	// Summarize pending pods
	s.printPodSummary("PENDING PODS", pendingPods)

	// Show what instance types could fit them
	s.analyzeInstanceTypesForPods(ctx, pendingPods)
}

// RunRescheduleAnalysis simulates rescheduling pods from a specific node
func (s *Simulator) RunRescheduleAnalysis(ctx context.Context, nodeName, excludeFamilies string) {
	slog.Info("analyzing reschedule scenario", "node", nodeName)

	// Get the target node
	node := &corev1.Node{}
	if err := s.kubeClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
		slog.Error("failed to get node", "node", nodeName, "error", err)
		return
	}

	// Get pods on this node
	pods, err := s.getPodsOnNode(ctx, nodeName)
	if err != nil {
		slog.Error("failed to get pods on node", "error", err)
		return
	}

	slog.Info("found pods on node", "node", nodeName, "count", len(pods))

	// Filter to Karpenter-managed pods (exclude daemonsets, static pods, etc.)
	reschedulablePods := lo.Filter(pods, func(p corev1.Pod, _ int) bool {
		// Skip daemonset pods
		for _, ref := range p.OwnerReferences {
			if ref.Kind == "DaemonSet" {
				return false
			}
		}
		// Skip mirror pods (static pods)
		if _, ok := p.Annotations["kubernetes.io/config.mirror"]; ok {
			return false
		}
		// Skip pods in kube-system by default
		if p.Namespace == "kube-system" {
			return false
		}
		return true
	})

	if len(reschedulablePods) == 0 {
		slog.Info("no reschedulable pods found on node")
		return
	}

	// Print current state
	s.printPodSummary(fmt.Sprintf("PODS ON NODE %s (would need rescheduling)", nodeName), reschedulablePods)

	// Analyze constraints
	s.printConstraintAnalysis(reschedulablePods)

	// Show what instance types could handle these pods
	s.analyzeInstanceTypesForPods(ctx, reschedulablePods)
}

// RunSyntheticAnalysis runs the original synthetic workload analysis
func (s *Simulator) RunSyntheticAnalysis(ctx context.Context, cpu, memory string, replicas int, excludeFam string, topN int) {
	slog.Info("running synthetic analysis",
		"cpu", cpu, "memory", memory, "replicas", replicas)

	cpuQty := resource.MustParse(cpu)
	memQty := resource.MustParse(memory)

	// Get instance types
	instanceTypes, err := s.cloudProvider.GetInstanceTypes(ctx, nil)
	if err != nil {
		slog.Error("failed to get instance types", "error", err)
		return
	}

	// Apply exclusions
	if excludeFam != "" {
		families := strings.Split(excludeFam, ",")
		instanceTypes = filterOutFamilies(instanceTypes, families)
	}

	// Filter to available
	instanceTypes = lo.Filter(instanceTypes, func(it *cloudprovider.InstanceType, _ int) bool {
		return len(it.Offerings.Available()) > 0
	})

	// Calculate bin packing
	type result struct {
		name        string
		podsPerNode int
		cpuUtil     float64
		memUtil     float64
		price       float64
	}

	var results []result
	for _, it := range instanceTypes {
		allocCPU := it.Allocatable()[corev1.ResourceCPU]
		allocMem := it.Allocatable()[corev1.ResourceMemory]

		if allocCPU.IsZero() || allocMem.IsZero() {
			continue
		}

		podsByCPU := allocCPU.MilliValue() / cpuQty.MilliValue()
		podsByMem := allocMem.Value() / memQty.Value()
		podsPerNode := int(min(podsByCPU, podsByMem))

		if podsPerNode <= 0 {
			continue
		}

		cpuUsed := cpuQty.MilliValue() * int64(podsPerNode)
		memUsed := memQty.Value() * int64(podsPerNode)

		results = append(results, result{
			name:        it.Name,
			podsPerNode: podsPerNode,
			cpuUtil:     float64(cpuUsed) / float64(allocCPU.MilliValue()) * 100,
			memUtil:     float64(memUsed) / float64(allocMem.Value()) * 100,
			price:       getLowestPrice(it),
		})
	}

	// Sort by cost per pod
	sort.Slice(results, func(i, j int) bool {
		return results[i].price/float64(results[i].podsPerNode) < results[j].price/float64(results[j].podsPerNode)
	})

	// Print results
	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Printf("SYNTHETIC WORKLOAD ANALYSIS: %d pods × (CPU: %s, Memory: %s)\n", replicas, cpu, memory)
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Println()
	fmt.Printf("%-20s %10s %10s %10s %12s %15s\n",
		"INSTANCE TYPE", "PODS/NODE", "CPU UTIL", "MEM UTIL", "$/HR", "$/POD/HR")
	fmt.Println(strings.Repeat("-", 100))

	for i, r := range results {
		if i >= topN {
			break
		}
		fmt.Printf("%-20s %10d %9.1f%% %9.1f%% %12.4f %15.6f\n",
			r.name, r.podsPerNode, r.cpuUtil, r.memUtil,
			r.price, r.price/float64(r.podsPerNode))
	}

	// Best choice summary
	if len(results) > 0 {
		best := results[0]
		nodesNeeded := (replicas + best.podsPerNode - 1) / best.podsPerNode
		totalCost := float64(nodesNeeded) * best.price

		fmt.Println()
		fmt.Println("RECOMMENDED:")
		fmt.Printf("  %s × %d nodes = %d pod capacity @ $%.4f/hr\n",
			best.name, nodesNeeded, nodesNeeded*best.podsPerNode, totalCost)
	}
}

// Helper methods

func (s *Simulator) getNodes(ctx context.Context, nodeName string) ([]corev1.Node, error) {
	if nodeName != "" {
		node := &corev1.Node{}
		if err := s.kubeClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			return nil, err
		}
		return []corev1.Node{*node}, nil
	}

	nodeList := &corev1.NodeList{}
	if err := s.kubeClient.List(ctx, nodeList); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

func (s *Simulator) getPods(ctx context.Context, ns, labelSel string) ([]corev1.Pod, error) {
	opts := []client.ListOption{}
	if ns != "" {
		opts = append(opts, client.InNamespace(ns))
	}
	if labelSel != "" {
		sel, err := labels.Parse(labelSel)
		if err != nil {
			return nil, fmt.Errorf("parsing label selector: %w", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}

	podList := &corev1.PodList{}
	if err := s.kubeClient.List(ctx, podList, opts...); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

func (s *Simulator) getPodsOnNode(ctx context.Context, nodeName string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := s.kubeClient.List(ctx, podList, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		// Field selector might not work, fall back to filtering
		if err := s.kubeClient.List(ctx, podList); err != nil {
			return nil, err
		}
	}

	return lo.Filter(podList.Items, func(p corev1.Pod, _ int) bool {
		return p.Spec.NodeName == nodeName
	}), nil
}

func (s *Simulator) buildNodeSummaries(nodes []corev1.Node, pods []corev1.Pod) []NodeSummary {
	// Group pods by node
	podsByNode := lo.GroupBy(pods, func(p corev1.Pod) string {
		return p.Spec.NodeName
	})

	var summaries []NodeSummary
	for _, node := range nodes {
		nodePods := podsByNode[node.Name]

		// Calculate allocated resources
		var allocCPU, allocMem resource.Quantity
		for _, pod := range nodePods {
			for _, c := range pod.Spec.Containers {
				if cpu := c.Resources.Requests.Cpu(); cpu != nil {
					allocCPU.Add(*cpu)
				}
				if mem := c.Resources.Requests.Memory(); mem != nil {
					allocMem.Add(*mem)
				}
			}
		}

		allocatable := node.Status.Allocatable
		cpuUtil := float64(allocCPU.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		memUtil := float64(allocMem.Value()) / float64(allocatable.Memory().Value()) * 100

		summaries = append(summaries, NodeSummary{
			Name:           node.Name,
			InstanceType:   node.Labels[corev1.LabelInstanceTypeStable],
			Zone:           node.Labels[corev1.LabelTopologyZone],
			CapacityType:   node.Labels[karpv1.CapacityTypeLabelKey],
			Allocatable:    allocatable,
			AllocatedCPU:   allocCPU,
			AllocatedMem:   allocMem,
			PodCount:       len(nodePods),
			CPUUtilization: cpuUtil,
			MemUtilization: memUtil,
		})
	}

	return summaries
}

func (s *Simulator) printEfficiencyReport(nodes []NodeSummary, pods []corev1.Pod) {
	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 119))
	fmt.Println("CLUSTER EFFICIENCY ANALYSIS")
	fmt.Println("=" + strings.Repeat("=", 119))
	fmt.Println()

	// Sort by utilization (lowest first to show waste)
	sort.Slice(nodes, func(i, j int) bool {
		return (nodes[i].CPUUtilization + nodes[i].MemUtilization) < (nodes[j].CPUUtilization + nodes[j].MemUtilization)
	})

	fmt.Printf("%-40s %-15s %-10s %8s %10s %10s %10s %10s\n",
		"NODE", "INSTANCE", "ZONE", "PODS", "CPU REQ", "CPU UTIL", "MEM REQ", "MEM UTIL")
	fmt.Println(strings.Repeat("-", 120))

	var totalCPUUtil, totalMemUtil float64
	for _, n := range nodes {
		fmt.Printf("%-40s %-15s %-10s %8d %10s %9.1f%% %10s %9.1f%%\n",
			truncate(n.Name, 40),
			n.InstanceType,
			n.Zone,
			n.PodCount,
			n.AllocatedCPU.String(),
			n.CPUUtilization,
			formatMemory(n.AllocatedMem),
			n.MemUtilization,
		)
		totalCPUUtil += n.CPUUtilization
		totalMemUtil += n.MemUtilization
	}

	fmt.Println(strings.Repeat("-", 120))

	if len(nodes) > 0 {
		avgCPU := totalCPUUtil / float64(len(nodes))
		avgMem := totalMemUtil / float64(len(nodes))
		fmt.Printf("%-40s %-15s %-10s %8d %10s %9.1f%% %10s %9.1f%%\n",
			"AVERAGE", "", "", 0, "", avgCPU, "", avgMem)
	}

	// Summary stats
	fmt.Println()
	fmt.Println("SUMMARY:")
	fmt.Printf("  Total Nodes: %d\n", len(nodes))
	fmt.Printf("  Total Pods:  %d\n", len(pods))

	// Find under-utilized nodes
	underUtilized := lo.Filter(nodes, func(n NodeSummary, _ int) bool {
		return n.CPUUtilization < 30 && n.MemUtilization < 30
	})
	if len(underUtilized) > 0 {
		fmt.Printf("  Under-utilized (<30%% CPU & MEM): %d nodes\n", len(underUtilized))
	}
}

func (s *Simulator) printPodSummary(title string, pods []corev1.Pod) {
	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Println(title)
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Println()

	// Aggregate by owner
	type ownerStats struct {
		kind              string
		name              string
		namespace         string
		count             int
		totalCPU          resource.Quantity
		totalMem          resource.Quantity
		hasAffinity       bool
		hasAntiAffinity   bool
		hasTopologySpread bool
	}

	byOwner := make(map[string]*ownerStats)
	for _, pod := range pods {
		var ownerKey, ownerKind, ownerName string
		if len(pod.OwnerReferences) > 0 {
			// Get the top-level owner (ReplicaSet -> Deployment)
			ownerKind = pod.OwnerReferences[0].Kind
			ownerName = pod.OwnerReferences[0].Name
			// Strip ReplicaSet hash suffix to group by Deployment
			if ownerKind == "ReplicaSet" {
				parts := strings.Split(ownerName, "-")
				if len(parts) > 1 {
					ownerName = strings.Join(parts[:len(parts)-1], "-")
					ownerKind = "Deployment"
				}
			}
		} else {
			ownerKind = "Pod"
			ownerName = pod.Name
		}
		ownerKey = fmt.Sprintf("%s/%s/%s", pod.Namespace, ownerKind, ownerName)

		stats, ok := byOwner[ownerKey]
		if !ok {
			stats = &ownerStats{
				kind:      ownerKind,
				name:      ownerName,
				namespace: pod.Namespace,
			}
			byOwner[ownerKey] = stats
		}

		stats.count++
		for _, c := range pod.Spec.Containers {
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				stats.totalCPU.Add(*cpu)
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				stats.totalMem.Add(*mem)
			}
		}

		if pod.Spec.Affinity != nil {
			if pod.Spec.Affinity.PodAffinity != nil {
				stats.hasAffinity = true
			}
			if pod.Spec.Affinity.PodAntiAffinity != nil {
				stats.hasAntiAffinity = true
			}
		}
		if len(pod.Spec.TopologySpreadConstraints) > 0 {
			stats.hasTopologySpread = true
		}
	}

	// Convert to slice and sort by count
	var statsList []*ownerStats
	for _, s := range byOwner {
		statsList = append(statsList, s)
	}
	sort.Slice(statsList, func(i, j int) bool {
		return statsList[i].count > statsList[j].count
	})

	fmt.Printf("%-12s %-30s %-15s %6s %10s %10s %s\n",
		"NAMESPACE", "OWNER", "KIND", "PODS", "CPU", "MEMORY", "CONSTRAINTS")
	fmt.Println(strings.Repeat("-", 100))

	for _, s := range statsList {
		constraints := []string{}
		if s.hasAffinity {
			constraints = append(constraints, "affinity")
		}
		if s.hasAntiAffinity {
			constraints = append(constraints, "anti-affinity")
		}
		if s.hasTopologySpread {
			constraints = append(constraints, "topo-spread")
		}
		constraintStr := strings.Join(constraints, ", ")
		if constraintStr == "" {
			constraintStr = "-"
		}

		fmt.Printf("%-12s %-30s %-15s %6d %10s %10s %s\n",
			truncate(s.namespace, 12),
			truncate(s.name, 30),
			s.kind,
			s.count,
			s.totalCPU.String(),
			formatMemory(s.totalMem),
			constraintStr,
		)
	}

	fmt.Printf("\nTotal: %d pods\n", len(pods))
}

func (s *Simulator) printConstraintAnalysis(pods []corev1.Pod) {
	fmt.Println()
	fmt.Println("SCHEDULING CONSTRAINTS ANALYSIS:")
	fmt.Println(strings.Repeat("-", 50))

	// Count constraint types
	var withAffinity, withAntiAffinity, withTopologySpread, withNodeSelector int
	topologyKeys := make(map[string]int)
	nodeSelectors := make(map[string]int)

	for _, pod := range pods {
		if pod.Spec.Affinity != nil {
			if pod.Spec.Affinity.PodAffinity != nil {
				withAffinity++
			}
			if pod.Spec.Affinity.PodAntiAffinity != nil {
				withAntiAffinity++
			}
		}
		if len(pod.Spec.TopologySpreadConstraints) > 0 {
			withTopologySpread++
			for _, tsc := range pod.Spec.TopologySpreadConstraints {
				topologyKeys[tsc.TopologyKey]++
			}
		}
		if len(pod.Spec.NodeSelector) > 0 {
			withNodeSelector++
			for k, v := range pod.Spec.NodeSelector {
				nodeSelectors[k+"="+v]++
			}
		}
	}

	fmt.Printf("  Pod Affinity:           %d pods\n", withAffinity)
	fmt.Printf("  Pod Anti-Affinity:      %d pods\n", withAntiAffinity)
	fmt.Printf("  Topology Spread:        %d pods\n", withTopologySpread)
	fmt.Printf("  Node Selector:          %d pods\n", withNodeSelector)

	if len(topologyKeys) > 0 {
		fmt.Println("\n  Topology Keys:")
		for k, count := range topologyKeys {
			fmt.Printf("    %s: %d pods\n", k, count)
		}
	}

	if len(nodeSelectors) > 0 {
		fmt.Println("\n  Node Selectors:")
		for k, count := range nodeSelectors {
			fmt.Printf("    %s: %d pods\n", k, count)
		}
	}
}

func (s *Simulator) analyzeInstanceTypesForPods(ctx context.Context, pods []corev1.Pod) {
	// Calculate total resources needed
	var totalCPU, totalMem resource.Quantity
	for _, pod := range pods {
		for _, c := range pod.Spec.Containers {
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				totalCPU.Add(*cpu)
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				totalMem.Add(*mem)
			}
		}
	}

	// Check for anti-affinity constraints that force 1 pod per node
	hasHostnameAntiAffinity := false
	for _, pod := range pods {
		if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAntiAffinity != nil {
			for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				if term.TopologyKey == corev1.LabelHostname {
					hasHostnameAntiAffinity = true
					break
				}
			}
		}
	}

	fmt.Println()
	fmt.Println("INSTANCE TYPE RECOMMENDATIONS:")
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  Total Resources Needed: CPU=%s, Memory=%s\n", totalCPU.String(), formatMemory(totalMem))
	fmt.Printf("  Pods: %d\n", len(pods))

	if hasHostnameAntiAffinity {
		fmt.Println()
		fmt.Println("  ⚠️  WARNING: Hostname anti-affinity detected!")
		fmt.Println("     Pods cannot be co-located - need 1 node per pod")
		fmt.Printf("     Minimum nodes required: %d\n", len(pods))
	}

	// Get instance types
	instanceTypes, err := s.cloudProvider.GetInstanceTypes(ctx, nil)
	if err != nil {
		slog.Error("failed to get instance types", "error", err)
		return
	}

	// Filter to available
	instanceTypes = lo.Filter(instanceTypes, func(it *cloudprovider.InstanceType, _ int) bool {
		return len(it.Offerings.Available()) > 0
	})

	// Calculate average pod size
	if len(pods) == 0 {
		return
	}
	avgCPU := totalCPU.MilliValue() / int64(len(pods))
	avgMem := totalMem.Value() / int64(len(pods))

	type suggestion struct {
		name        string
		podsPerNode int
		nodesNeeded int
		price       float64
		totalCost   float64
	}

	var suggestions []suggestion
	for _, it := range instanceTypes {
		allocCPU := it.Allocatable()[corev1.ResourceCPU]
		allocMem := it.Allocatable()[corev1.ResourceMemory]

		if allocCPU.IsZero() || allocMem.IsZero() {
			continue
		}

		var podsPerNode int
		if hasHostnameAntiAffinity {
			podsPerNode = 1
		} else {
			podsByCPU := allocCPU.MilliValue() / avgCPU
			podsByMem := allocMem.Value() / avgMem
			podsPerNode = int(min(podsByCPU, podsByMem))
		}

		if podsPerNode <= 0 {
			continue
		}

		nodesNeeded := (len(pods) + podsPerNode - 1) / podsPerNode
		price := getLowestPrice(it)
		totalCost := float64(nodesNeeded) * price

		suggestions = append(suggestions, suggestion{
			name:        it.Name,
			podsPerNode: podsPerNode,
			nodesNeeded: nodesNeeded,
			price:       price,
			totalCost:   totalCost,
		})
	}

	// Sort by total cost
	sort.Slice(suggestions, func(i, j int) bool {
		return suggestions[i].totalCost < suggestions[j].totalCost
	})

	fmt.Println()
	fmt.Printf("%-20s %12s %12s %12s %15s\n",
		"INSTANCE TYPE", "PODS/NODE", "NODES", "$/HR/NODE", "TOTAL $/HR")
	fmt.Println(strings.Repeat("-", 75))

	for i, s := range suggestions {
		if i >= 10 {
			break
		}
		fmt.Printf("%-20s %12d %12d %12.4f %15.4f\n",
			s.name, s.podsPerNode, s.nodesNeeded, s.price, s.totalCost)
	}

	if len(suggestions) > 0 {
		best := suggestions[0]
		fmt.Println()
		fmt.Printf("BEST CHOICE: %s × %d = $%.4f/hr\n",
			best.name, best.nodesNeeded, best.totalCost)
	}
}

// Utility functions

func filterOutFamilies(instanceTypes []*cloudprovider.InstanceType, families []string) []*cloudprovider.InstanceType {
	return lo.Filter(instanceTypes, func(it *cloudprovider.InstanceType, _ int) bool {
		family := strings.Split(it.Name, ".")[0]
		return !lo.Contains(families, family)
	})
}

func getLowestPrice(it *cloudprovider.InstanceType) float64 {
	offerings := it.Offerings.Available()
	if len(offerings) == 0 {
		return 0
	}
	minPrice := offerings[0].Price
	for _, o := range offerings {
		if o.Price < minPrice {
			minPrice = o.Price
		}
	}
	return minPrice
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func formatMemory(q resource.Quantity) string {
	bytes := q.Value()
	if bytes >= 1024*1024*1024 {
		return fmt.Sprintf("%.1fGi", float64(bytes)/(1024*1024*1024))
	}
	if bytes >= 1024*1024 {
		return fmt.Sprintf("%.0fMi", float64(bytes)/(1024*1024))
	}
	return q.String()
}

// SnapshotSimulator works with offline snapshot data
type SnapshotSimulator struct {
	snapshot *simulation.ClusterSnapshot
}

// RunEfficiencyAnalysis analyzes cluster efficiency from snapshot data
func (s *SnapshotSimulator) RunEfficiencyAnalysis(ns, labelSel, nodeName string) {
	slog.Info("running offline efficiency analysis")

	// Filter nodes
	nodes := s.getNodes(nodeName)

	// Filter pods
	pods := s.getPods(ns, labelSel)

	// Build node summaries
	nodeSummaries := s.buildNodeSummaries(nodes, pods)

	// Print analysis
	s.printEfficiencyReport(nodeSummaries, pods)
}

// RunPendingPodsAnalysis shows pending pods from snapshot
func (s *SnapshotSimulator) RunPendingPodsAnalysis(ns, labelSel string) {
	slog.Info("analyzing pending pods from snapshot")

	pods := s.getPods(ns, labelSel)
	pendingPods := lo.Filter(pods, func(p simulation.PodSnapshot, _ int) bool {
		return p.Status.Phase == corev1.PodPending && p.Spec.NodeName == ""
	})

	if len(pendingPods) == 0 {
		slog.Info("no pending pods found")
		return
	}

	slog.Info("found pending pods", "count", len(pendingPods))
	s.printPodSummary("PENDING PODS", pendingPods)
	s.analyzeInstanceTypesForPods(pendingPods)
}

// RunRescheduleAnalysis simulates rescheduling from a node
func (s *SnapshotSimulator) RunRescheduleAnalysis(nodeName, excludeFam string) {
	slog.Info("analyzing reschedule scenario from snapshot", "node", nodeName)

	// Get node info
	node := s.snapshot.GetNode(nodeName)
	if node == nil {
		slog.Error("node not found in snapshot", "node", nodeName)
		return
	}

	// Get pods on this node
	pods := lo.Filter(s.snapshot.Pods, func(p simulation.PodSnapshot, _ int) bool {
		return p.Spec.NodeName == nodeName
	})

	// Filter to reschedulable pods
	reschedulablePods := lo.Filter(pods, func(p simulation.PodSnapshot, _ int) bool {
		// Skip daemonset pods
		for _, ref := range p.Metadata.OwnerReferences {
			if ref.Kind == "DaemonSet" {
				return false
			}
		}
		// Skip kube-system by default
		if p.Metadata.Namespace == "kube-system" {
			return false
		}
		return true
	})

	if len(reschedulablePods) == 0 {
		slog.Info("no reschedulable pods found on node")
		return
	}

	s.printPodSummary(fmt.Sprintf("PODS ON NODE %s (would need rescheduling)", nodeName), reschedulablePods)
	s.printConstraintAnalysis(reschedulablePods)
	s.analyzeInstanceTypesForPods(reschedulablePods)
}

// RunSyntheticAnalysis runs synthetic workload analysis
func (s *SnapshotSimulator) RunSyntheticAnalysis(cpu, memory string, replicas int, excludeFam string, topN int) {
	slog.Info("running synthetic analysis from snapshot",
		"cpu", cpu, "memory", memory, "replicas", replicas)

	cpuQty := resource.MustParse(cpu)
	memQty := resource.MustParse(memory)

	// Get instance types from snapshot
	instanceTypes := s.getAvailableInstanceTypes()

	// Apply exclusions
	if excludeFam != "" {
		families := strings.Split(excludeFam, ",")
		instanceTypes = lo.Filter(instanceTypes, func(it simulation.InstanceTypeSnapshot, _ int) bool {
			family := strings.Split(it.Name, ".")[0]
			return !lo.Contains(families, family)
		})
	}

	// Calculate bin packing
	type result struct {
		name        string
		podsPerNode int
		cpuUtil     float64
		memUtil     float64
		price       float64
	}

	var results []result
	for _, it := range instanceTypes {
		allocCPU := resource.MustParse(it.CPU)
		allocMem := resource.MustParse(it.Memory)

		if allocCPU.IsZero() || allocMem.IsZero() {
			continue
		}

		podsByCPU := allocCPU.MilliValue() / cpuQty.MilliValue()
		podsByMem := allocMem.Value() / memQty.Value()
		podsPerNode := int(min(podsByCPU, podsByMem))

		if podsPerNode <= 0 {
			continue
		}

		cpuUsed := cpuQty.MilliValue() * int64(podsPerNode)
		memUsed := memQty.Value() * int64(podsPerNode)

		results = append(results, result{
			name:        it.Name,
			podsPerNode: podsPerNode,
			cpuUtil:     float64(cpuUsed) / float64(allocCPU.MilliValue()) * 100,
			memUtil:     float64(memUsed) / float64(allocMem.Value()) * 100,
			price:       s.getLowestPrice(it),
		})
	}

	// Sort by cost per pod
	sort.Slice(results, func(i, j int) bool {
		return results[i].price/float64(results[i].podsPerNode) < results[j].price/float64(results[j].podsPerNode)
	})

	// Print results
	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Printf("SYNTHETIC WORKLOAD ANALYSIS: %d pods × (CPU: %s, Memory: %s)\n", replicas, cpu, memory)
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Println()
	fmt.Printf("%-20s %10s %10s %10s %12s %15s\n",
		"INSTANCE TYPE", "PODS/NODE", "CPU UTIL", "MEM UTIL", "$/HR", "$/POD/HR")
	fmt.Println(strings.Repeat("-", 100))

	for i, r := range results {
		if i >= topN {
			break
		}
		fmt.Printf("%-20s %10d %9.1f%% %9.1f%% %12.4f %15.6f\n",
			r.name, r.podsPerNode, r.cpuUtil, r.memUtil,
			r.price, r.price/float64(r.podsPerNode))
	}

	if len(results) > 0 {
		best := results[0]
		nodesNeeded := (replicas + best.podsPerNode - 1) / best.podsPerNode
		totalCost := float64(nodesNeeded) * best.price

		fmt.Println()
		fmt.Println("RECOMMENDED:")
		fmt.Printf("  %s × %d nodes = %d pod capacity @ $%.4f/hr\n",
			best.name, nodesNeeded, nodesNeeded*best.podsPerNode, totalCost)
	}
}

// Helper methods for SnapshotSimulator

func (s *SnapshotSimulator) getNodes(nodeName string) []simulation.NodeSnapshot {
	if nodeName != "" {
		if node := s.snapshot.GetNode(nodeName); node != nil {
			return []simulation.NodeSnapshot{*node}
		}
		return nil
	}
	return s.snapshot.Nodes
}

func (s *SnapshotSimulator) getPods(ns, labelSel string) []simulation.PodSnapshot {
	pods := s.snapshot.Pods

	if ns != "" {
		pods = lo.Filter(pods, func(p simulation.PodSnapshot, _ int) bool {
			return p.Metadata.Namespace == ns
		})
	}

	if labelSel != "" {
		sel, err := labels.Parse(labelSel)
		if err != nil {
			slog.Warn("invalid label selector", "selector", labelSel)
			return pods
		}
		pods = lo.Filter(pods, func(p simulation.PodSnapshot, _ int) bool {
			return sel.Matches(labels.Set(p.Metadata.Labels))
		})
	}

	return pods
}

func (s *SnapshotSimulator) getAvailableInstanceTypes() []simulation.InstanceTypeSnapshot {
	return lo.Filter(s.snapshot.InstanceTypes, func(it simulation.InstanceTypeSnapshot, _ int) bool {
		// Check if any offering is available
		for _, o := range it.Offerings {
			if o.Available {
				return true
			}
		}
		return false
	})
}

func (s *SnapshotSimulator) getLowestPrice(it simulation.InstanceTypeSnapshot) float64 {
	available := lo.Filter(it.Offerings, func(o simulation.OfferingSnapshot, _ int) bool {
		return o.Available
	})
	if len(available) == 0 {
		return 0
	}
	return lo.MinBy(available, func(a, b simulation.OfferingSnapshot) bool {
		return a.Price < b.Price
	}).Price
}

func (s *SnapshotSimulator) buildNodeSummaries(nodes []simulation.NodeSnapshot, pods []simulation.PodSnapshot) []NodeSummary {
	// Group pods by node
	podsByNode := lo.GroupBy(pods, func(p simulation.PodSnapshot) string {
		return p.Spec.NodeName
	})

	var summaries []NodeSummary
	for _, node := range nodes {
		nodePods := podsByNode[node.Name]

		// Calculate allocated resources
		var allocCPU, allocMem resource.Quantity
		for _, pod := range nodePods {
			for _, c := range pod.Spec.Containers {
				if cpu := c.Resources.Requests.Cpu(); cpu != nil {
					allocCPU.Add(*cpu)
				}
				if mem := c.Resources.Requests.Memory(); mem != nil {
					allocMem.Add(*mem)
				}
			}
		}

		allocatable := node.Allocatable
		cpuUtil := float64(allocCPU.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		memUtil := float64(allocMem.Value()) / float64(allocatable.Memory().Value()) * 100

		summaries = append(summaries, NodeSummary{
			Name:           node.Name,
			InstanceType:   node.Labels[corev1.LabelInstanceTypeStable],
			Zone:           node.Labels[corev1.LabelTopologyZone],
			CapacityType:   node.Labels[karpv1.CapacityTypeLabelKey],
			Allocatable:    allocatable,
			AllocatedCPU:   allocCPU,
			AllocatedMem:   allocMem,
			PodCount:       len(nodePods),
			CPUUtilization: cpuUtil,
			MemUtilization: memUtil,
		})
	}

	return summaries
}

func (s *SnapshotSimulator) printEfficiencyReport(nodes []NodeSummary, pods []simulation.PodSnapshot) {
	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 119))
	fmt.Println("CLUSTER EFFICIENCY ANALYSIS (from snapshot)")
	fmt.Println("=" + strings.Repeat("=", 119))
	fmt.Println()

	// Sort by utilization (lowest first to show waste)
	sort.Slice(nodes, func(i, j int) bool {
		return (nodes[i].CPUUtilization + nodes[i].MemUtilization) < (nodes[j].CPUUtilization + nodes[j].MemUtilization)
	})

	fmt.Printf("%-40s %-15s %-10s %8s %10s %10s %10s %10s\n",
		"NODE", "INSTANCE", "ZONE", "PODS", "CPU REQ", "CPU UTIL", "MEM REQ", "MEM UTIL")
	fmt.Println(strings.Repeat("-", 120))

	var totalCPUUtil, totalMemUtil float64
	for _, n := range nodes {
		fmt.Printf("%-40s %-15s %-10s %8d %10s %9.1f%% %10s %9.1f%%\n",
			truncate(n.Name, 40),
			n.InstanceType,
			n.Zone,
			n.PodCount,
			n.AllocatedCPU.String(),
			n.CPUUtilization,
			formatMemory(n.AllocatedMem),
			n.MemUtilization,
		)
		totalCPUUtil += n.CPUUtilization
		totalMemUtil += n.MemUtilization
	}

	fmt.Println(strings.Repeat("-", 120))

	if len(nodes) > 0 {
		avgCPU := totalCPUUtil / float64(len(nodes))
		avgMem := totalMemUtil / float64(len(nodes))
		fmt.Printf("%-40s %-15s %-10s %8d %10s %9.1f%% %10s %9.1f%%\n",
			"AVERAGE", "", "", 0, "", avgCPU, "", avgMem)
	}

	// Summary stats
	fmt.Println()
	fmt.Println("SUMMARY:")
	fmt.Printf("  Total Nodes: %d\n", len(nodes))
	fmt.Printf("  Total Pods:  %d\n", len(pods))

	// Find under-utilized nodes
	underUtilized := lo.Filter(nodes, func(n NodeSummary, _ int) bool {
		return n.CPUUtilization < 30 && n.MemUtilization < 30
	})
	if len(underUtilized) > 0 {
		fmt.Printf("  Under-utilized (<30%% CPU & MEM): %d nodes\n", len(underUtilized))
	}
}

func (s *SnapshotSimulator) printPodSummary(title string, pods []simulation.PodSnapshot) {
	fmt.Println()
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Println(title)
	fmt.Println("=" + strings.Repeat("=", 99))
	fmt.Println()

	// Aggregate by owner
	type ownerStats struct {
		kind              string
		name              string
		namespace         string
		count             int
		totalCPU          resource.Quantity
		totalMem          resource.Quantity
		hasAffinity       bool
		hasAntiAffinity   bool
		hasTopologySpread bool
	}

	byOwner := make(map[string]*ownerStats)
	for _, pod := range pods {
		var ownerKey, ownerKind, ownerName string
		if len(pod.Metadata.OwnerReferences) > 0 {
			ownerKind = pod.Metadata.OwnerReferences[0].Kind
			ownerName = pod.Metadata.OwnerReferences[0].Name
			// Strip ReplicaSet hash suffix to group by Deployment
			if ownerKind == "ReplicaSet" {
				parts := strings.Split(ownerName, "-")
				if len(parts) > 1 {
					ownerName = strings.Join(parts[:len(parts)-1], "-")
					ownerKind = "Deployment"
				}
			}
		} else {
			ownerKind = "Pod"
			ownerName = pod.Metadata.Name
		}
		ownerKey = fmt.Sprintf("%s/%s/%s", pod.Metadata.Namespace, ownerKind, ownerName)

		stats, ok := byOwner[ownerKey]
		if !ok {
			stats = &ownerStats{
				kind:      ownerKind,
				name:      ownerName,
				namespace: pod.Metadata.Namespace,
			}
			byOwner[ownerKey] = stats
		}

		stats.count++
		for _, c := range pod.Spec.Containers {
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				stats.totalCPU.Add(*cpu)
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				stats.totalMem.Add(*mem)
			}
		}

		if pod.Spec.Affinity != nil {
			if pod.Spec.Affinity.PodAffinity != nil {
				stats.hasAffinity = true
			}
			if pod.Spec.Affinity.PodAntiAffinity != nil {
				stats.hasAntiAffinity = true
			}
		}
		if len(pod.Spec.TopologySpreadConstraints) > 0 {
			stats.hasTopologySpread = true
		}
	}

	// Convert to slice and sort by count
	var statsList []*ownerStats
	for _, st := range byOwner {
		statsList = append(statsList, st)
	}
	sort.Slice(statsList, func(i, j int) bool {
		return statsList[i].count > statsList[j].count
	})

	fmt.Printf("%-12s %-30s %-15s %6s %10s %10s %s\n",
		"NAMESPACE", "OWNER", "KIND", "PODS", "CPU", "MEMORY", "CONSTRAINTS")
	fmt.Println(strings.Repeat("-", 100))

	for _, st := range statsList {
		constraints := []string{}
		if st.hasAffinity {
			constraints = append(constraints, "affinity")
		}
		if st.hasAntiAffinity {
			constraints = append(constraints, "anti-affinity")
		}
		if st.hasTopologySpread {
			constraints = append(constraints, "topo-spread")
		}
		constraintStr := strings.Join(constraints, ", ")
		if constraintStr == "" {
			constraintStr = "-"
		}

		fmt.Printf("%-12s %-30s %-15s %6d %10s %10s %s\n",
			truncate(st.namespace, 12),
			truncate(st.name, 30),
			st.kind,
			st.count,
			st.totalCPU.String(),
			formatMemory(st.totalMem),
			constraintStr,
		)
	}

	fmt.Printf("\nTotal: %d pods\n", len(pods))
}

func (s *SnapshotSimulator) printConstraintAnalysis(pods []simulation.PodSnapshot) {
	fmt.Println()
	fmt.Println("SCHEDULING CONSTRAINTS ANALYSIS:")
	fmt.Println(strings.Repeat("-", 50))

	var withAffinity, withAntiAffinity, withTopologySpread, withNodeSelector int
	topologyKeys := make(map[string]int)
	nodeSelectors := make(map[string]int)

	for _, pod := range pods {
		if pod.Spec.Affinity != nil {
			if pod.Spec.Affinity.PodAffinity != nil {
				withAffinity++
			}
			if pod.Spec.Affinity.PodAntiAffinity != nil {
				withAntiAffinity++
			}
		}
		if len(pod.Spec.TopologySpreadConstraints) > 0 {
			withTopologySpread++
			for _, tsc := range pod.Spec.TopologySpreadConstraints {
				topologyKeys[tsc.TopologyKey]++
			}
		}
		if len(pod.Spec.NodeSelector) > 0 {
			withNodeSelector++
			for k, v := range pod.Spec.NodeSelector {
				nodeSelectors[k+"="+v]++
			}
		}
	}

	fmt.Printf("  Pod Affinity:           %d pods\n", withAffinity)
	fmt.Printf("  Pod Anti-Affinity:      %d pods\n", withAntiAffinity)
	fmt.Printf("  Topology Spread:        %d pods\n", withTopologySpread)
	fmt.Printf("  Node Selector:          %d pods\n", withNodeSelector)

	if len(topologyKeys) > 0 {
		fmt.Println("\n  Topology Keys:")
		for k, count := range topologyKeys {
			fmt.Printf("    %s: %d pods\n", k, count)
		}
	}

	if len(nodeSelectors) > 0 {
		fmt.Println("\n  Node Selectors:")
		for k, count := range nodeSelectors {
			fmt.Printf("    %s: %d pods\n", k, count)
		}
	}
}

func (s *SnapshotSimulator) analyzeInstanceTypesForPods(pods []simulation.PodSnapshot) {
	// Calculate total resources needed
	var totalCPU, totalMem resource.Quantity
	for _, pod := range pods {
		for _, c := range pod.Spec.Containers {
			if cpu := c.Resources.Requests.Cpu(); cpu != nil {
				totalCPU.Add(*cpu)
			}
			if mem := c.Resources.Requests.Memory(); mem != nil {
				totalMem.Add(*mem)
			}
		}
	}

	// Check for anti-affinity constraints
	hasHostnameAntiAffinity := false
	for _, pod := range pods {
		if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAntiAffinity != nil {
			for _, term := range pod.Spec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
				if term.TopologyKey == corev1.LabelHostname {
					hasHostnameAntiAffinity = true
					break
				}
			}
		}
	}

	fmt.Println()
	fmt.Println("INSTANCE TYPE RECOMMENDATIONS:")
	fmt.Println(strings.Repeat("-", 50))
	fmt.Printf("  Total Resources Needed: CPU=%s, Memory=%s\n", totalCPU.String(), formatMemory(totalMem))
	fmt.Printf("  Pods: %d\n", len(pods))

	if hasHostnameAntiAffinity {
		fmt.Println()
		fmt.Println("  WARNING: Hostname anti-affinity detected!")
		fmt.Println("     Pods cannot be co-located - need 1 node per pod")
		fmt.Printf("     Minimum nodes required: %d\n", len(pods))
	}

	// Get available instance types
	instanceTypes := s.getAvailableInstanceTypes()

	if len(pods) == 0 {
		return
	}
	avgCPU := totalCPU.MilliValue() / int64(len(pods))
	avgMem := totalMem.Value() / int64(len(pods))

	type suggestion struct {
		name        string
		podsPerNode int
		nodesNeeded int
		price       float64
		totalCost   float64
	}

	var suggestions []suggestion
	for _, it := range instanceTypes {
		allocCPU := resource.MustParse(it.CPU)
		allocMem := resource.MustParse(it.Memory)

		if allocCPU.IsZero() || allocMem.IsZero() {
			continue
		}

		var podsPerNode int
		if hasHostnameAntiAffinity {
			podsPerNode = 1
		} else {
			podsByCPU := allocCPU.MilliValue() / avgCPU
			podsByMem := allocMem.Value() / avgMem
			podsPerNode = int(min(podsByCPU, podsByMem))
		}

		if podsPerNode <= 0 {
			continue
		}

		nodesNeeded := (len(pods) + podsPerNode - 1) / podsPerNode
		price := s.getLowestPrice(it)
		totalCost := float64(nodesNeeded) * price

		suggestions = append(suggestions, suggestion{
			name:        it.Name,
			podsPerNode: podsPerNode,
			nodesNeeded: nodesNeeded,
			price:       price,
			totalCost:   totalCost,
		})
	}

	// Sort by total cost
	sort.Slice(suggestions, func(i, j int) bool {
		return suggestions[i].totalCost < suggestions[j].totalCost
	})

	fmt.Println()
	fmt.Printf("%-20s %12s %12s %12s %15s\n",
		"INSTANCE TYPE", "PODS/NODE", "NODES", "$/HR/NODE", "TOTAL $/HR")
	fmt.Println(strings.Repeat("-", 75))

	for i, sg := range suggestions {
		if i >= 10 {
			break
		}
		fmt.Printf("%-20s %12d %12d %12.4f %15.4f\n",
			sg.name, sg.podsPerNode, sg.nodesNeeded, sg.price, sg.totalCost)
	}

	if len(suggestions) > 0 {
		best := suggestions[0]
		fmt.Println()
		fmt.Printf("BEST CHOICE: %s × %d = $%.4f/hr\n",
			best.name, best.nodesNeeded, best.totalCost)
	}
}
