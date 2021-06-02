package csidriveroperator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/labels"
	coreinformersv1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corelistersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	operatorapi "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/status"
	"github.com/openshift/library-go/pkg/operator/v1helpers"

	"github.com/openshift/cluster-storage-operator/pkg/csoclients"
	"github.com/openshift/cluster-storage-operator/pkg/operator/configobservation/util"
	"github.com/openshift/cluster-storage-operator/pkg/operator/csidriveroperator/csioperatorclient"
	csoutils "github.com/openshift/cluster-storage-operator/pkg/utils"
)

// This CSIDriverStarterController installs and syncs CSI driver operator Deployment.
// It replace ${LOG_LEVEL} in the Deployment with current log level.
// It replaces images in the Deployment using  CSIOperatorConfig.ImageReplacer.
// It produces following Conditions:
// <CSI driver name>CSIDriverOperatorDeploymentAvailable
// <CSI driver name>CSIDriverOperatorDeploymentProgressing
// <CSI driver name>CSIDriverOperatorDeploymentDegraded
type CSIDriverOperatorDeploymentController struct {
	name              string
	deploymentAsset   string
	operatorClient    v1helpers.OperatorClient
	csiOperatorConfig csioperatorclient.CSIOperatorConfig
	kubeClient        kubernetes.Interface
	nodeLister        corelistersv1.NodeLister
	versionGetter     status.VersionGetter
	targetVersion     string
	eventRecorder     events.Recorder
	factory           *factory.Factory
}

var _ factory.Controller = &CSIDriverOperatorDeploymentController{}

const (
	deploymentControllerName = "CSIDriverOperatorDeployment"
)

func NewCSIDriverOperatorDeploymentController(
	clients *csoclients.Clients,
	nodeInformer coreinformersv1.NodeInformer,
	csiOperatorConfig csioperatorclient.CSIOperatorConfig,
	versionGetter status.VersionGetter,
	targetVersion string,
	eventRecorder events.Recorder,
	resyncInterval time.Duration,
) factory.Controller {
	f := factory.New()
	f = f.ResyncEvery(resyncInterval)
	f = f.WithSyncDegradedOnError(clients.OperatorClient)
	// Necessary to do initial Sync after the controller starts.
	f = f.WithPostStartHooks(initalSync)
	// Add informers to the factory now, but the actual event handlers
	// are added later in CSIDriverOperatorDeploymentController.Run(),
	// when we're 100% sure the controller is going to start (because it
	// depends on the platform).
	// If we added the event handlers now, all events would pile up in the
	// controller queue, without anything reading it.
	f = f.WithInformers(
		clients.OperatorClient.Informer(),
		clients.KubeInformers.InformersFor("").Core().V1().Nodes().Informer(),
		clients.KubeInformers.InformersFor(csoclients.CSIOperatorNamespace).Apps().V1().Deployments().Informer())

	c := &CSIDriverOperatorDeploymentController{
		name:              csiOperatorConfig.ConditionPrefix,
		operatorClient:    clients.OperatorClient,
		csiOperatorConfig: csiOperatorConfig,
		kubeClient:        clients.KubeClient,
		nodeLister:        clients.KubeInformers.InformersFor("").Core().V1().Nodes().Lister(),
		versionGetter:     versionGetter,
		targetVersion:     targetVersion,
		eventRecorder:     eventRecorder.WithComponentSuffix(csiOperatorConfig.ConditionPrefix),
		factory:           f,
	}
	return c
}

func (c *CSIDriverOperatorDeploymentController) Sync(ctx context.Context, syncCtx factory.SyncContext) error {
	klog.V(4).Infof("CSIDriverOperatorDeploymentController sync started")
	defer klog.V(4).Infof("CSIDriverOperatorDeploymentController sync finished")

	opSpec, opStatus, _, err := c.operatorClient.GetOperatorState()
	if err != nil {
		return err
	}
	if opSpec.ManagementState != operatorapi.Managed {
		return nil
	}

	if !olmRemovalComplete(c.csiOperatorConfig, opStatus) {
		// Wait for the OLM driver to be removed first.
		// OLMOperatorRemovalController already reports its own progress, so
		// users know what's going on.
		return nil
	}

	replacers := []*strings.Replacer{sidecarReplacer}
	// Replace images
	if c.csiOperatorConfig.ImageReplacer != nil {
		replacers = append(replacers, c.csiOperatorConfig.ImageReplacer)
	}

	required := csoutils.GetRequiredDeployment(c.csiOperatorConfig.DeploymentAsset, opSpec, replacers...)
	requiredCopy, err := util.InjectObservedProxyInDeploymentContainers(required, opSpec)
	if err != nil {
		return fmt.Errorf("failed to inject proxy data into deployment: %w", err)
	}

	// Set the number of replicas according to the number of nodes available
	nodeSelector := requiredCopy.Spec.Template.Spec.NodeSelector
	nodes, err := c.nodeLister.List(labels.SelectorFromSet(nodeSelector))
	if err != nil {
		// This will set Degraded condition
		return err
	}

	// Set the deployment.Spec.Replicas field according to the number
	// of available nodes. If the number of available nodes is bigger
	// than 1, then the number of replicas will be 2.
	replicas := int32(1)
	if len(nodes) > 1 {
		replicas = int32(2)
	}
	requiredCopy.Spec.Replicas = &replicas

	_, err = csoutils.CreateDeployment(csoutils.DeploymentOptions{
		Required:       requiredCopy,
		ControllerName: c.Name(),
		OpStatus:       opStatus,
		EventRecorder:  c.eventRecorder,
		KubeClient:     c.kubeClient,
		OperatorClient: c.operatorClient,
		TargetVersion:  c.targetVersion,
		VersionGetter:  c.versionGetter,
		VersionName:    c.name + versionName,
	})
	return err
}

func (c *CSIDriverOperatorDeploymentController) Run(ctx context.Context, workers int) {
	// This adds event handlers to informers.
	ctrl := c.factory.WithSync(c.Sync).ToController(c.Name(), c.eventRecorder)
	ctrl.Run(ctx, workers)
}

func (c *CSIDriverOperatorDeploymentController) Name() string {
	return c.name + deploymentControllerName
}
