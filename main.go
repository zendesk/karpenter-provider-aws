package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	awsoperator "github.com/aws/karpenter-provider-aws/pkg/operator"
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

	// Load kubeconfig from default location
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: "sandbox",
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	// Get the rest config
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		log.Fatalf("Failed to load kubeconfig: %v", err)
	}

	// Create a controller-runtime client
	kubeClient, err := client.New(restConfig, client.Options{})
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	fmt.Println("Connected to 'sandbox' context")

	// Create clock
	clk := clock.RealClock{}

	// Initialize core operator
	ctx, coreOp := coreoperator.NewOperator()

	// Initialize AWS operator with all providers
	ctx, op := awsoperator.NewOperator(ctx, coreOp)

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
