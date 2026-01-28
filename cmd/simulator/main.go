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

// simulator runs a live KWOK cluster simulation with Karpenter.
// It loads a snapshot into a KWOK cluster and runs the actual Karpenter
// controllers, allowing you to test provisioning and disruption behavior.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	corecontrollers "sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
	karpoptions "sigs.k8s.io/karpenter/pkg/operator/options"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"

	v1 "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
	"github.com/aws/karpenter-provider-aws/pkg/controllers"
	"github.com/aws/karpenter-provider-aws/pkg/simulation"

	"github.com/aws/karpenter-provider-aws/kwok/cloudprovider"
	"github.com/aws/karpenter-provider-aws/kwok/operator"

	// Import for side-effect: registers types to scheme.Scheme
	_ "github.com/aws/karpenter-provider-aws/pkg/apis/v1"
)

var (
	snapshotFile      string
	instanceTypesFile string
	clusterName       string
	contextName       string
	dryRun            bool
)

func init() {
	flag.StringVar(&snapshotFile, "from", "", "path to snapshot file (required)")
	flag.StringVar(&instanceTypesFile, "with-instances", "", "path to instance types file (required)")
	flag.StringVar(&clusterName, "cluster", "kwok", "kwokctl cluster name")
	flag.StringVar(&contextName, "context", "", "kubeconfig context (default: kwok-<cluster>)")
	flag.BoolVar(&dryRun, "dry-run", false, "only load resources, don't start Karpenter")
}

func main() {
	flag.Parse()

	// Handle subcommand
	args := flag.Args()
	if len(args) == 0 || args[0] != "run" {
		fmt.Println("Usage: simulator --from <snapshot.yml> --with-instances <instances.yaml> run")
		fmt.Println()
		fmt.Println("Flags:")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if snapshotFile == "" || instanceTypesFile == "" {
		slog.Error("--from and --with-instances are required")
		flag.Usage()
		os.Exit(1)
	}

	// Setup logging
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Setup context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		slog.Error("simulator failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// 1. Load snapshot
	slog.Info("loading snapshot", "path", snapshotFile)
	snapshot, err := simulation.LoadSnapshot(snapshotFile)
	if err != nil {
		return fmt.Errorf("loading snapshot: %w", err)
	}
	slog.Info("snapshot loaded",
		"ec2NodeClasses", len(snapshot.EC2NodeClasses),
		"nodePools", len(snapshot.NodePools),
		"nodeClaims", len(snapshot.NodeClaims),
		"pods", len(snapshot.Pods),
	)

	// 2. Load instance types
	slog.Info("loading instance types", "path", instanceTypesFile)
	instanceTypes, err := simulation.LoadInstanceTypeCatalog(instanceTypesFile)
	if err != nil {
		return fmt.Errorf("loading instance types: %w", err)
	}
	slog.Info("instance types loaded",
		"count", len(instanceTypes.InstanceTypes),
		"region", instanceTypes.Metadata.Region,
	)

	// 3. Connect to KWOK cluster
	if contextName == "" {
		contextName = fmt.Sprintf("kwok-%s", clusterName)
	}
	slog.Info("connecting to KWOK cluster", "context", contextName)

	kubeClient, restConfig, err := createKubeClient(contextName)
	if err != nil {
		return fmt.Errorf("connecting to cluster: %w", err)
	}

	// Test connection
	if _, err := kubeClient.ServerVersion(); err != nil {
		return fmt.Errorf("failed to connect to cluster: %w", err)
	}
	slog.Info("connected to KWOK cluster")

	// 4. Create controller-runtime client
	ctrlClient, err := client.New(restConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("creating controller client: %w", err)
	}

	// 5. Create resources from snapshot
	slog.Info("creating resources in cluster...")
	if err := createResources(ctx, ctrlClient, snapshot); err != nil {
		return fmt.Errorf("creating resources: %w", err)
	}

	if dryRun {
		slog.Info("dry-run mode: resources created, not starting Karpenter")
		return nil
	}

	// 6. Start Karpenter with KWOK cloud provider
	slog.Info("starting Karpenter...")

	// Set AWS_REGION from instance types metadata so KWOK operator doesn't try to use IMDS
	os.Setenv("AWS_REGION", instanceTypes.Metadata.Region)

	return runKarpenter(ctx)
}

func createKubeClient(kubeContext string) (*kubernetes.Clientset, *rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: kubeContext,
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("loading kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("creating clientset: %w", err)
	}

	return clientset, restConfig, nil
}

func createResources(ctx context.Context, c client.Client, snapshot *simulation.ClusterSnapshot) error {
	// Create EC2NodeClasses
	slog.Info("creating EC2NodeClasses", "count", len(snapshot.EC2NodeClasses))
	for i := range snapshot.EC2NodeClasses {
		nc := snapshot.EC2NodeClasses[i].DeepCopy()
		// Clear status and resource version for creation
		nc.Status = v1.EC2NodeClassStatus{}
		nc.ResourceVersion = ""
		nc.UID = ""
		nc.CreationTimestamp = metav1.Time{}
		nc.ManagedFields = nil
		// Remove finalizers to allow clean creation
		nc.Finalizers = nil

		if err := c.Create(ctx, nc); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				slog.Debug("EC2NodeClass already exists", "name", nc.Name)
				continue
			}
			return fmt.Errorf("creating EC2NodeClass %s: %w", nc.Name, err)
		}
		slog.Debug("created EC2NodeClass", "name", nc.Name)
	}
	slog.Info("EC2NodeClasses created")

	// Create NodePools
	slog.Info("creating NodePools", "count", len(snapshot.NodePools))
	for i := range snapshot.NodePools {
		np := snapshot.NodePools[i].DeepCopy()
		// Clear status and resource version for creation
		np.Status = karpv1.NodePoolStatus{}
		np.ResourceVersion = ""
		np.UID = ""
		np.CreationTimestamp = metav1.Time{}
		np.ManagedFields = nil
		np.Finalizers = nil

		if err := c.Create(ctx, np); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				slog.Debug("NodePool already exists", "name", np.Name)
				continue
			}
			return fmt.Errorf("creating NodePool %s: %w", np.Name, err)
		}
		slog.Debug("created NodePool", "name", np.Name)
	}
	slog.Info("NodePools created")

	// Create NodeClaims (these will trigger the KWOK EC2 client to create fake instances)
	slog.Info("creating NodeClaims", "count", len(snapshot.NodeClaims))
	for i := range snapshot.NodeClaims {
		ncl := snapshot.NodeClaims[i].DeepCopy()
		// Clear status and resource version for creation
		ncl.Status = karpv1.NodeClaimStatus{}
		ncl.ResourceVersion = ""
		ncl.UID = ""
		ncl.CreationTimestamp = metav1.Time{}
		ncl.ManagedFields = nil
		ncl.Finalizers = nil

		if err := c.Create(ctx, ncl); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				slog.Debug("NodeClaim already exists", "name", ncl.Name)
				continue
			}
			return fmt.Errorf("creating NodeClaim %s: %w", ncl.Name, err)
		}
		slog.Debug("created NodeClaim", "name", ncl.Name)
	}
	slog.Info("NodeClaims created")

	// Create Nodes (KWOK will simulate these nodes)
	slog.Info("creating Nodes", "count", len(snapshot.Nodes))
	for i := range snapshot.Nodes {
		nodeSnapshot := &snapshot.Nodes[i]
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   nodeSnapshot.Name,
				Labels: nodeSnapshot.Labels,
			},
			Spec: corev1.NodeSpec{
				ProviderID: nodeSnapshot.ProviderID,
				Taints:     nodeSnapshot.Taints,
			},
		}

		if err := c.Create(ctx, node); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				slog.Debug("Node already exists", "name", node.Name)
				continue
			}
			return fmt.Errorf("creating Node %s: %w", node.Name, err)
		}
		slog.Debug("created Node", "name", node.Name)
	}
	slog.Info("Nodes created")

	// Create PriorityClasses (before pods since pods may reference them)
	slog.Info("creating PriorityClasses", "count", len(snapshot.PriorityClasses))
	for i := range snapshot.PriorityClasses {
		pc := snapshot.PriorityClasses[i].DeepCopy()
		// Clear resource version for creation
		pc.ResourceVersion = ""
		pc.UID = ""
		pc.CreationTimestamp = metav1.Time{}
		pc.ManagedFields = nil

		if err := c.Create(ctx, pc); err != nil {
			if k8serrors.IsAlreadyExists(err) {
				slog.Debug("PriorityClass already exists", "name", pc.Name)
				continue
			}
			return fmt.Errorf("creating PriorityClass %s: %w", pc.Name, err)
		}
		slog.Debug("created PriorityClass", "name", pc.Name)
	}
	slog.Info("PriorityClasses created")

	// Pre-create all namespaces first
	namespaces := make(map[string]bool)
	for i := range snapshot.Pods {
		namespaces[snapshot.Pods[i].Metadata.Namespace] = true
	}
	slog.Info("creating namespaces", "count", len(namespaces))
	for ns := range namespaces {
		nsObj := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}
		if err := c.Create(ctx, nsObj); err != nil && !k8serrors.IsAlreadyExists(err) {
			slog.Warn("failed to create namespace", "namespace", ns, "error", err)
		}
	}

	// Create Pods in parallel with bounded concurrency
	slog.Info("creating Pods", "count", len(snapshot.Pods))
	const maxConcurrency = 50
	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	var created, skipped int64

	for i := range snapshot.Pods {
		wg.Add(1)
		sem <- struct{}{} // acquire semaphore

		go func(podSnapshot simulation.PodSnapshot) {
			defer wg.Done()
			defer func() { <-sem }() // release semaphore

			pod := podSnapshot.ToCorePod()
			// Clear status and resource version for creation
			pod.Status = corev1.PodStatus{}
			pod.ResourceVersion = ""
			pod.UID = ""
			pod.CreationTimestamp = metav1.Time{}
			pod.ManagedFields = nil
			// Clear node assignment so pods need to be scheduled
			pod.Spec.NodeName = ""

			if err := c.Create(ctx, pod); err != nil {
				if k8serrors.IsAlreadyExists(err) {
					atomic.AddInt64(&skipped, 1)
					return
				}
				slog.Debug("failed to create pod", "name", pod.Name, "namespace", pod.Namespace, "error", err)
				atomic.AddInt64(&skipped, 1)
				return
			}
			atomic.AddInt64(&created, 1)
		}(snapshot.Pods[i])
	}
	wg.Wait()
	slog.Info("Pods created", "created", created, "skipped", skipped)

	return nil
}

func runKarpenter(ctx context.Context) error {
	// Set required environment variables for KWOK operator
	os.Setenv("SYSTEM_NAMESPACE", "karpenter")
	os.Setenv("DISABLE_LEADER_ELECTION", "true")
	os.Setenv("CLUSTER_NAME", clusterName)
	os.Setenv("CLUSTER_ENDPOINT", "https://localhost:6443") // KWOK uses localhost

	// Store instance types in environment for KWOK ec2 client to use
	// The KWOK ec2 client will need to be modified to read from file
	os.Setenv("KWOK_INSTANCE_TYPES_FILE", instanceTypesFile)

	// Set command line args for Karpenter
	originalArgs := os.Args
	os.Args = []string{
		"karpenter",
		fmt.Sprintf("--cluster-name=%s", clusterName),
		"--cluster-endpoint=https://localhost:6443",
	}
	defer func() { os.Args = originalArgs }()

	// Create core operator and KWOK AWS operator
	// Note: coreoperator.NewOperator() returns (context.Context, *Operator)
	ctx, op := operator.NewOperator(coreoperator.NewOperator())

	// Create KWOK AWS cloud provider
	kwokAWSCloudProvider := cloudprovider.New(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		op.EventRecorder,
		op.GetClient(),
		op.AMIProvider,
		op.SecurityGroupProvider,
		op.CapacityReservationProvider,
		op.InstanceTypeStore,
	)
	overlayUndecoratedCloudProvider := metrics.Decorate(kwokAWSCloudProvider)
	cloudProvider := overlay.Decorate(overlayUndecoratedCloudProvider, op.GetClient(), op.InstanceTypeStore)
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	// Enable capacity reservations if feature gate is on
	if karpoptions.FromContext(ctx).FeatureGates.ReservedCapacity {
		v1.CapacityReservationsEnabled = true
	}

	// Start background threads
	wg := &sync.WaitGroup{}
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-op.Elected()
		op.EC2API.StartBackupThread(ctx)
	}()
	go func() {
		defer wg.Done()
		<-op.Elected()
		op.EC2API.StartKillNodeThread(ctx)
	}()
	go func() {
		defer wg.Done()
		<-op.Elected()
		op.EC2API.ReadBackup(ctx)
	}()

	slog.Info("Karpenter starting...")
	slog.Info("simulation running - press Ctrl+C to stop")

	// Start operator with controllers
	op.
		WithControllers(ctx, corecontrollers.NewControllers(
			ctx,
			op.Manager,
			op.Clock,
			op.GetClient(),
			op.EventRecorder,
			cloudProvider,
			overlayUndecoratedCloudProvider,
			clusterState,
			op.InstanceTypeStore,
		)...).
		WithControllers(ctx, controllers.NewControllers(
			ctx,
			op.Manager,
			op.Config,
			op.Clock,
			op.EC2API,
			op.GetClient(),
			op.EventRecorder,
			op.UnavailableOfferingsCache,
			op.SSMCache,
			op.ValidationCache,
			op.RecreationCache,
			cloudProvider,
			op.SubnetProvider,
			op.SecurityGroupProvider,
			op.InstanceProfileProvider,
			op.InstanceProvider,
			op.PricingProvider,
			op.AMIProvider,
			op.LaunchTemplateProvider,
			op.VersionProvider,
			op.InstanceTypesProvider,
			op.CapacityReservationProvider,
			op.AMIResolver,
		)...).
		Start(ctx)

	wg.Wait()
	return nil
}
