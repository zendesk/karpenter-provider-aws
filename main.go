package main

import (
	"context"
	"fmt"
	"os"

	_ "github.com/aws/karpenter-provider-aws/pkg/apis/v1" // for init
	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/operator"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme" // need this for the init()
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
)

func main() {
	ctx := context.Background()

	validateContext()

	// Create a logger that outputs to stdout
	logger := zap.New(zap.WriteTo(os.Stdout), zap.UseDevMode(true))
	ctx = log.IntoContext(ctx, logger)

	// Disable leader election for local development
	os.Setenv("DISABLE_LEADER_ELECTION", "true")

	// Add cluster endpoint flag for operator.NewOperator to read
	os.Args = append(os.Args, "-cluster-endpoint=https://kubernetes.default.svc.cluster.local./")

	// stolen from hack/tools/allocatable_diff/main.go
	ctx, op := operator.NewOperator(coreoperator.NewOperator())
	fmt.Println("X")

	// Start the manager's cache before using the client
	go func() {
		lo.Must0(op.Manager.GetCache().Start(ctx))
	}()

	// Wait for the cache to sync
	lo.Must0(op.Manager.GetCache().WaitForCacheSync(ctx))

	// Use the operator's client to verify we can list nodes and NodeClaims
	kubeClient := op.GetClient()

	// Debug: Check if NodeClaim is registered in the scheme
	gvks, _, err := scheme.Scheme.ObjectKinds(&karpv1.NodeClaim{})
	if err != nil {
		fmt.Printf("Error getting GVKs for NodeClaim: %v\n", err)
	} else {
		fmt.Printf("NodeClaim registered with GVKs: %v\n", gvks)
	}

	nodeList := &corev1.NodeList{}
	lo.Must0(kubeClient.List(ctx, nodeList))
	fmt.Printf("Found %d nodes\n", len(nodeList.Items))

	nodeClaimList := &karpv1.NodeClaimList{}
	lo.Must0(kubeClient.List(ctx, nodeClaimList))
	fmt.Printf("Found %d nodeclaims\n", len(nodeClaimList.Items))

	// Create clock
	clk := clock.RealClock{}

	// Create AWS cloud provider
	awsCloudProvider := cloudprovider.New(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		op.EventRecorder,
		op.GetClient(),
		op.AMIProvider,
		op.SecurityGroupProvider,
		op.CapacityReservationProvider,
		op.InstanceTypeStore,
	)
	overlayUndecoratedCloudProvider := metrics.Decorate(awsCloudProvider)
	cp := overlay.Decorate(overlayUndecoratedCloudProvider, op.GetClient(), op.InstanceTypeStore)

	// Create state cluster
	cluster := state.NewCluster(clk, kubeClient, cp)

	// Create event recorder (using a fake recorder for testing)
	recorder := events.NewRecorder(&record.FakeRecorder{})

	// Create provisioner (stub - in real usage this would need full initialization)
	// For this test, we'll pass nil and expect it to handle gracefully or error
	var provisioner *provisioning.Provisioner = nil

	// Create disruption queue
	queue := disruption.NewQueue(kubeClient, recorder, cluster, clk, provisioner)

	// Create disruption controller
	controller := disruption.NewController(
		clk,
		kubeClient,
		provisioner,
		cp,
		recorder,
		cluster,
		queue,
	)

	fmt.Println("Created disruption controller")

	// Run Reconcile once
	fmt.Println("Running Reconcile...")
	result, err := controller.Reconcile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Reconcile failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Reconcile completed successfully\n")
	fmt.Printf("Result: RequeueAfter=%v, Requeue=%v\n", result.RequeueAfter, result.Requeue)
}

// Verify the current kubectl context is "sandbox" since users have to use use-context because NewOperator calls ctrl.GetConfigOrDie()
func validateContext() {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	rawConfig, err := kubeConfig.RawConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading kubeconfig: %v\n", err)
		os.Exit(1)
	}
	if rawConfig.CurrentContext != "sandbox" {
		fmt.Fprintf(os.Stderr, "Error: Current kubectl context is '%s', but must be 'sandbox'\n", rawConfig.CurrentContext)
		fmt.Fprintf(os.Stderr, "Run: kubectl config use-context sandbox\n")
		os.Exit(1)
	}
}
