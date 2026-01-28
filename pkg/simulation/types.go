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

package simulation

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
)

// ClusterSnapshot captures the complete state of a cluster for offline simulation.
// This format is designed to be human-readable (YAML) and contain everything needed
// for bin packing analysis without requiring cluster or AWS API access.
type ClusterSnapshot struct {
	// APIVersion for the snapshot format
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	// Kind is always "ClusterSnapshot"
	Kind string `json:"kind" yaml:"kind"`
	// Metadata about when/where the snapshot was captured
	Metadata SnapshotMetadata `json:"metadata" yaml:"metadata"`

	// InstanceTypes available in this region with pricing information
	InstanceTypes []InstanceTypeSnapshot `json:"instanceTypes" yaml:"instanceTypes"`
	// Nodes currently in the cluster (Karpenter-managed only)
	Nodes []NodeSnapshot `json:"nodes" yaml:"nodes"`
	// Pods with full scheduling constraints
	Pods []PodSnapshot `json:"pods" yaml:"pods"`
	// NodePools define instance requirements and constraints
	NodePools []karpv1.NodePool `json:"nodePools" yaml:"nodePools"`
	// EC2NodeClasses with resolved subnet/security group information
	EC2NodeClasses []v1.EC2NodeClass `json:"ec2NodeClasses" yaml:"ec2NodeClasses"`
	// NodeClaims link nodes to nodepools
	NodeClaims []karpv1.NodeClaim `json:"nodeClaims" yaml:"nodeClaims"`
	// PriorityClasses define pod scheduling priorities
	PriorityClasses []schedulingv1.PriorityClass `json:"priorityClasses,omitempty" yaml:"priorityClasses,omitempty"`
}

// SnapshotMetadata contains information about when and where the snapshot was captured.
type SnapshotMetadata struct {
	// CapturedAt is the timestamp when the snapshot was created
	CapturedAt time.Time `json:"capturedAt" yaml:"capturedAt"`
	// ClusterName is the name of the EKS cluster
	ClusterName string `json:"clusterName" yaml:"clusterName"`
	// Region is the AWS region
	Region string `json:"region" yaml:"region"`
	// Context is the kubectl context used to capture the snapshot
	Context string `json:"context,omitempty" yaml:"context,omitempty"`
	// KarpenterVersion is the version of Karpenter running in the cluster
	KarpenterVersion string `json:"karpenterVersion,omitempty" yaml:"karpenterVersion,omitempty"`
}

// InstanceTypeSnapshot captures instance type information for offline simulation.
// This data comes from AWS DescribeInstanceTypes and pricing APIs.
type InstanceTypeSnapshot struct {
	// Name is the instance type name (e.g., "m5.large")
	Name string `json:"name" yaml:"name"`
	// CPU is the number of vCPUs
	CPU string `json:"cpu" yaml:"cpu"`
	// Memory in Kubernetes resource format (e.g., "8Gi")
	Memory string `json:"memory" yaml:"memory"`
	// Architecture is either "amd64" or "arm64"
	Architecture string `json:"architecture" yaml:"architecture"`
	// Offerings are the available zones and capacity types with pricing
	Offerings []OfferingSnapshot `json:"offerings" yaml:"offerings"`
	// Capacity contains additional resource information
	Capacity ResourceSnapshot `json:"capacity,omitempty" yaml:"capacity,omitempty"`
}

// OfferingSnapshot represents availability and pricing for a specific zone/capacity-type combination.
type OfferingSnapshot struct {
	// Zone is the availability zone (e.g., "us-west-2a")
	Zone string `json:"zone" yaml:"zone"`
	// CapacityType is "on-demand", "spot", or "reserved"
	CapacityType string `json:"capacityType" yaml:"capacityType"`
	// Price is the hourly cost in USD
	Price float64 `json:"price" yaml:"price"`
	// Available indicates if this offering is currently available
	Available bool `json:"available" yaml:"available"`
}

// ResourceSnapshot captures resource capacity beyond CPU/Memory.
type ResourceSnapshot struct {
	// EphemeralStorage in Kubernetes resource format
	EphemeralStorage string `json:"ephemeralStorage,omitempty" yaml:"ephemeralStorage,omitempty"`
	// Pods is the maximum number of pods
	Pods string `json:"pods,omitempty" yaml:"pods,omitempty"`
	// GPU resources if any
	NvidiaGPU string `json:"nvidia.com/gpu,omitempty" yaml:"nvidia.com/gpu,omitempty"`
	AMDGPU    string `json:"amd.com/gpu,omitempty" yaml:"amd.com/gpu,omitempty"`
	// AWS-specific resources
	AWSNeuron string `json:"aws.amazon.com/neuron,omitempty" yaml:"aws.amazon.com/neuron,omitempty"`
	EFA       string `json:"vpc.amazonaws.com/efa,omitempty" yaml:"vpc.amazonaws.com/efa,omitempty"`
}

// NodeSnapshot captures node information relevant to scheduling.
// Only nodes with a corresponding NodeClaim (Karpenter-managed) are included.
type NodeSnapshot struct {
	// Name is the node name
	Name string `json:"name" yaml:"name"`
	// Labels on the node (includes instance type, zone, arch, etc.)
	Labels map[string]string `json:"labels" yaml:"labels"`
	// ProviderID links to the cloud instance
	ProviderID string `json:"providerID,omitempty" yaml:"providerID,omitempty"`
	// Taints on the node
	Taints []corev1.Taint `json:"taints,omitempty" yaml:"taints,omitempty"`
	// Allocatable resources available for pods
	Allocatable corev1.ResourceList `json:"allocatable" yaml:"allocatable"`
}

// PodSnapshot captures pod information relevant to scheduling decisions.
// Only scheduling-relevant fields are included - no secrets, volumes, etc.
type PodSnapshot struct {
	// Metadata contains name, namespace, labels, and owner references
	Metadata PodMetadataSnapshot `json:"metadata" yaml:"metadata"`
	// Spec contains scheduling constraints and resource requests
	Spec PodSpecSnapshot `json:"spec" yaml:"spec"`
	// Status contains current scheduling status
	Status PodStatusSnapshot `json:"status" yaml:"status"`
}

// PodMetadataSnapshot contains pod identity and ownership information.
type PodMetadataSnapshot struct {
	// Name of the pod
	Name string `json:"name" yaml:"name"`
	// Namespace of the pod
	Namespace string `json:"namespace" yaml:"namespace"`
	// Labels on the pod (used for affinity matching)
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	// Annotations (only karpenter.sh/* annotations are captured)
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	// OwnerReferences identify the controller (Deployment, StatefulSet, etc.)
	OwnerReferences []metav1.OwnerReference `json:"ownerReferences,omitempty" yaml:"ownerReferences,omitempty"`
}

// PodSpecSnapshot contains scheduling constraints and resource requirements.
type PodSpecSnapshot struct {
	// NodeName is set if the pod is already scheduled
	NodeName string `json:"nodeName,omitempty" yaml:"nodeName,omitempty"`
	// Containers with resource requests/limits
	Containers []ContainerSnapshot `json:"containers" yaml:"containers"`
	// InitContainers with resource requests/limits
	InitContainers []ContainerSnapshot `json:"initContainers,omitempty" yaml:"initContainers,omitempty"`
	// NodeSelector constraints
	NodeSelector map[string]string `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`
	// Affinity rules (pod affinity, anti-affinity, node affinity)
	Affinity *corev1.Affinity `json:"affinity,omitempty" yaml:"affinity,omitempty"`
	// TopologySpreadConstraints for zone/hostname spreading
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty" yaml:"topologySpreadConstraints,omitempty"`
	// Tolerations for node taints
	Tolerations []corev1.Toleration `json:"tolerations,omitempty" yaml:"tolerations,omitempty"`
	// PriorityClassName affects preemption
	PriorityClassName string `json:"priorityClassName,omitempty" yaml:"priorityClassName,omitempty"`
	// Priority value
	Priority *int32 `json:"priority,omitempty" yaml:"priority,omitempty"`
	// RuntimeClassName for special runtime requirements
	RuntimeClassName *string `json:"runtimeClassName,omitempty" yaml:"runtimeClassName,omitempty"`
}

// ContainerSnapshot contains resource requests and limits for a container.
type ContainerSnapshot struct {
	// Name of the container
	Name string `json:"name" yaml:"name"`
	// Resources contains requests and limits
	Resources corev1.ResourceRequirements `json:"resources" yaml:"resources"`
}

// PodStatusSnapshot contains current scheduling status.
type PodStatusSnapshot struct {
	// Phase is Running, Pending, etc.
	Phase corev1.PodPhase `json:"phase" yaml:"phase"`
	// Conditions may indicate scheduling issues
	Conditions []corev1.PodCondition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// NewClusterSnapshot creates a new empty ClusterSnapshot with proper defaults.
func NewClusterSnapshot(clusterName, region string) *ClusterSnapshot {
	return &ClusterSnapshot{
		APIVersion: "simulation.karpenter.sh/v1alpha1",
		Kind:       "ClusterSnapshot",
		Metadata: SnapshotMetadata{
			CapturedAt:  time.Now(),
			ClusterName: clusterName,
			Region:      region,
		},
		InstanceTypes:   []InstanceTypeSnapshot{},
		Nodes:           []NodeSnapshot{},
		Pods:            []PodSnapshot{},
		NodePools:       []karpv1.NodePool{},
		EC2NodeClasses:  []v1.EC2NodeClass{},
		NodeClaims:      []karpv1.NodeClaim{},
		PriorityClasses: []schedulingv1.PriorityClass{},
	}
}

// GetTotalResources returns the total requested resources across all pods.
func (s *ClusterSnapshot) GetTotalResources() (cpu, memory resource.Quantity) {
	for _, pod := range s.Pods {
		for _, container := range pod.Spec.Containers {
			if cpuReq := container.Resources.Requests.Cpu(); cpuReq != nil {
				cpu.Add(*cpuReq)
			}
			if memReq := container.Resources.Requests.Memory(); memReq != nil {
				memory.Add(*memReq)
			}
		}
		for _, container := range pod.Spec.InitContainers {
			// Init containers are not additive, take max
			if cpuReq := container.Resources.Requests.Cpu(); cpuReq != nil && cpuReq.Cmp(cpu) > 0 {
				// Init containers run sequentially, so we just need to account for the largest
			}
		}
	}
	return cpu, memory
}

// GetPodsByNode returns pods grouped by their current node name.
func (s *ClusterSnapshot) GetPodsByNode() map[string][]PodSnapshot {
	result := make(map[string][]PodSnapshot)
	for _, pod := range s.Pods {
		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			nodeName = "<pending>"
		}
		result[nodeName] = append(result[nodeName], pod)
	}
	return result
}

// GetPendingPods returns pods that are not yet scheduled to a node.
func (s *ClusterSnapshot) GetPendingPods() []PodSnapshot {
	var pending []PodSnapshot
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == "" && pod.Status.Phase == corev1.PodPending {
			pending = append(pending, pod)
		}
	}
	return pending
}

// GetInstanceType returns instance type info by name.
func (s *ClusterSnapshot) GetInstanceType(name string) *InstanceTypeSnapshot {
	for i := range s.InstanceTypes {
		if s.InstanceTypes[i].Name == name {
			return &s.InstanceTypes[i]
		}
	}
	return nil
}

// GetNode returns node info by name.
func (s *ClusterSnapshot) GetNode(name string) *NodeSnapshot {
	for i := range s.Nodes {
		if s.Nodes[i].Name == name {
			return &s.Nodes[i]
		}
	}
	return nil
}

// sanitizeTopologySpreadConstraints removes matchLabelKeys entries that
// already exist in the labelSelector to avoid K8s validation errors.
// This is necessary because K8s 1.27+ rejects TSCs where the same key
// appears in both matchLabelKeys and labelSelector.
func sanitizeTopologySpreadConstraints(constraints []corev1.TopologySpreadConstraint) []corev1.TopologySpreadConstraint {
	if len(constraints) == 0 {
		return constraints
	}

	result := make([]corev1.TopologySpreadConstraint, len(constraints))
	for i, tsc := range constraints {
		result[i] = *tsc.DeepCopy()

		if result[i].LabelSelector == nil || len(result[i].MatchLabelKeys) == 0 {
			continue
		}

		// Collect keys from labelSelector
		selectorKeys := make(map[string]bool)
		for k := range result[i].LabelSelector.MatchLabels {
			selectorKeys[k] = true
		}
		for _, expr := range result[i].LabelSelector.MatchExpressions {
			selectorKeys[expr.Key] = true
		}

		// Filter matchLabelKeys to remove duplicates
		var filteredKeys []string
		for _, key := range result[i].MatchLabelKeys {
			if !selectorKeys[key] {
				filteredKeys = append(filteredKeys, key)
			}
		}
		result[i].MatchLabelKeys = filteredKeys
	}
	return result
}

// ToCorePod converts a PodSnapshot to a core v1 Pod suitable for creation in a cluster.
func (p *PodSnapshot) ToCorePod() *corev1.Pod {
	// Convert containers
	containers := make([]corev1.Container, len(p.Spec.Containers))
	for i, c := range p.Spec.Containers {
		containers[i] = corev1.Container{
			Name:      c.Name,
			Image:     "registry.k8s.io/pause:3.9", // Use pause image for simulation
			Resources: c.Resources,
		}
	}

	initContainers := make([]corev1.Container, len(p.Spec.InitContainers))
	for i, c := range p.Spec.InitContainers {
		initContainers[i] = corev1.Container{
			Name:      c.Name,
			Image:     "registry.k8s.io/pause:3.9",
			Resources: c.Resources,
		}
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            p.Metadata.Name,
			Namespace:       p.Metadata.Namespace,
			Labels:          p.Metadata.Labels,
			Annotations:     p.Metadata.Annotations,
			OwnerReferences: p.Metadata.OwnerReferences,
		},
		Spec: corev1.PodSpec{
			Containers:                containers,
			InitContainers:            initContainers,
			NodeSelector:              p.Spec.NodeSelector,
			Affinity:                  p.Spec.Affinity,
			TopologySpreadConstraints: sanitizeTopologySpreadConstraints(p.Spec.TopologySpreadConstraints),
			Tolerations:               p.Spec.Tolerations,
			PriorityClassName:         p.Spec.PriorityClassName,
			Priority:                  p.Spec.Priority,
			RuntimeClassName:          p.Spec.RuntimeClassName,
		},
	}
}
