package main

import (
	"fmt"
	"os"

	_ "github.com/aws/karpenter-provider-aws/pkg/apis/v1" // for init
	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/operator"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/metrics"
	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/nodeoverlay"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoperator "sigs.k8s.io/karpenter/pkg/operator"
)

// supported env vars via kubernetes grosser/local branch:
// - CONSOLIDATABLE=ignore to not check the status
// - NODEPOOLPREFIX=node-usw2a to focus on a single nodepool
// - CMETHOD=multi|single to run only 1 method

// PrintingRecorder wraps FakeRecorder and prints each event
type PrintingRecorder struct {
	*record.FakeRecorder
}

func (r *PrintingRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	objectName := getObjectName(object)
	fmt.Printf("EVENT: object=%s type=%s reason=%s message=%s\n", objectName, eventtype, reason, message)
	r.FakeRecorder.Event(object, eventtype, reason, message)
}

func (r *PrintingRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	objectName := getObjectName(object)
	message := fmt.Sprintf(messageFmt, args...)
	fmt.Printf("EVENT: object=%s type=%s reason=%s message=%s\n", objectName, eventtype, reason, message)
	r.FakeRecorder.Eventf(object, eventtype, reason, messageFmt, args...)
}

func (r *PrintingRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	objectName := getObjectName(object)
	message := fmt.Sprintf(messageFmt, args...)
	fmt.Printf("EVENT: object=%s type=%s reason=%s message=%s annotations=%v\n", objectName, eventtype, reason, message, annotations)
	r.FakeRecorder.AnnotatedEventf(object, annotations, eventtype, reason, messageFmt, args...)
}

func getObjectName(object runtime.Object) string {
	if object == nil {
		return "<nil>"
	}
	accessor, err := meta.Accessor(object)
	if err != nil {
		return fmt.Sprintf("<error: %v>", err)
	}
	name := accessor.GetName()
	namespace := accessor.GetNamespace()
	if namespace != "" {
		return fmt.Sprintf("%s/%s", namespace, name)
	}
	return name
}

func main() {
	clusterName := os.Getenv("CLUSTER")
	if clusterName == "" {
		fmt.Fprintf(os.Stderr, "CLUSTER env var must be set\n")
		os.Exit(1)
	}
	if os.Getenv("AWS_REGION") == "" {
		fmt.Fprintf(os.Stderr, "AWS_REGION env var must be set\n")
		os.Exit(1)
	}

	validateContext(clusterName)

	// Disable leader election for local development
	os.Setenv("DISABLE_LEADER_ELECTION", "true")

	// Configure logging to stdout
	os.Setenv("LOG_LEVEL", "debug")
	os.Setenv("LOG_OUTPUT_PATHS", "stdout")
	os.Setenv("LOG_ERROR_OUTPUT_PATHS", "stderr")

	// we ignore preferred affinity in compute-karpenter
	os.Setenv("PREFERENCE_POLICY", "Ignore")

	// set the same features as compute-karpenter does
	os.Setenv("FEATURE_GATES", "ReservedCapacity=false,SpotToSpotConsolidation=false,NodeRepair=false,NodeOverlay=true,StaticCapacity=false")

	// Add cluster endpoint flag for operator.NewOperator to read
	os.Args = append(os.Args, "-cluster-endpoint=https://kubernetes.default.svc.cluster.local./")
	os.Args = append(os.Args, fmt.Sprintf("-cluster-name=%v", clusterName))

	// stolen from hack/tools/allocatable_diff/main.go
	ctx, op := operator.NewOperator(coreoperator.NewOperator())

	// Start the manager's cache before using the client
	go func() {
		lo.Must0(op.Manager.GetCache().Start(ctx))
	}()

	// Wait for the cache to sync
	lo.Must0(op.Manager.GetCache().WaitForCacheSync(ctx))

	// Use the operator's client to verify we can list nodes and NodeClaims
	kubeClient := op.GetClient()

	// Create clock
	clk := clock.RealClock{}

	// Create noop event recorder for local development
	noopRecorder := events.NewRecorder(&PrintingRecorder{FakeRecorder: &record.FakeRecorder{}})

	// Create AWS cloud provider
	rawAwsCloudProvider := cloudprovider.New(
		op.InstanceTypesProvider,
		op.InstanceProvider,
		noopRecorder,
		op.GetClient(),
		op.AMIProvider,
		op.SecurityGroupProvider,
		op.CapacityReservationProvider,
		op.InstanceTypeStore,
	)
	cp := metrics.Decorate(rawAwsCloudProvider)
	cp = overlay.Decorate(cp, op.GetClient(), op.InstanceTypeStore)

	// Create state cluster
	cluster := state.NewCluster(clk, kubeClient, cp)

	nodeList := &corev1.NodeList{}
	lo.Must0(kubeClient.List(ctx, nodeList))
	for _, node := range nodeList.Items {
		err := cluster.UpdateNode(ctx, &node)
		noErr(err, fmt.Sprintf("Error updating node %v"))
	}

	nodeClaimList := &karpv1.NodeClaimList{}
	lo.Must0(kubeClient.List(ctx, nodeClaimList))
	for _, nc := range nodeClaimList.Items {
		cluster.UpdateNodeClaim(&nc)
	}

	// TODO: maybe also UpdateDaemonSet ... check if any exist in cache first

	// Create event recorder (using a fake recorder for testing)
	recorder := events.NewRecorder(&PrintingRecorder{FakeRecorder: &record.FakeRecorder{}})

	var provisioner = provisioning.NewProvisioner(
		kubeClient,
		recorder,
		cp,
		cluster,
		clk,
	)

	// Mark nodeoverlays as valid so reconciler does not complain
	// TODO: this should not be needed, but somehow reconcile only works when overlays are valid, which makes no sense since it is what makes them valid
	nodePoolList := &karpv1.NodePoolList{}
	err := kubeClient.List(ctx, nodePoolList)
	noErr(err, "listing nodepools")
	op.InstanceTypeStore.EvaluatedNodePools().Insert(lo.Map(nodePoolList.Items, func(np karpv1.NodePool, _ int) string {
		return np.Name
	})...)

	// Seed nodeoverlays (can confirm they are applied by checking IsPriceOverlaid in BuildNodePoolMap, should be true for every type)
	overlayController := nodeoverlay.NewController(
		kubeClient,
		cp,
		op.InstanceTypeStore,
		cluster,
	)
	_, err = overlayController.Reconcile(ctx, reconcile.Request{})
	noErr(err, "seeding nodeoverlays")

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
	noErr(err, "reconciliation failed")
	fmt.Printf("Reconcile completed successfully\n")
	fmt.Printf("Result: RequeueAfter=%v, Requeue=%v\n", result.RequeueAfter, result.Requeue)
}

func noErr(err error, msg string) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
		os.Exit(1)
	}
}

// Verify the current kubectl context is correct since users have to use use-context because NewOperator calls ctrl.GetConfigOrDie()
func validateContext(name string) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	rawConfig, err := kubeConfig.RawConfig()
	noErr(err, "Error loading kubeconfig")
	if rawConfig.CurrentContext != name {
		fmt.Fprintf(os.Stderr, "Error: Current kubectl context is '%s', but must be '%s'\n", rawConfig.CurrentContext, name)
		fmt.Fprintf(os.Stderr, "Run: kubectl config use-context %s\n", name)
		os.Exit(1)
	}
}
