package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lb_controller/internal/crusoe"
	utils "lb_controller/internal/utils"
	"strconv"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/go-logr/logr"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type opStatus string

const (
	AnnotationHealthCheckFailureCount          = "crusoe.ai/health-check-failure-count"
	AnnotationHealthCheckInterval              = "crusoe.ai/health-check-interval"
	AnnotationHealthCheckSuccessCount          = "crusoe.ai/health-check-success-count"
	AnnotationHealthCheckTimeout               = "crusoe.ai/health-check-timeout"
	projectIDEnvKey                            = "CRUSOE_PROJECT_ID"
	projectIDLabelKey                          = "crusoe.ai/project.id"
	instanceIDEnvKey                           = "CRUSOE_INSTANCE_ID"
	instanceIDLabelKey                         = "crusoe.ai/instance.id"
	loadbalancerIDLabelKey                     = "crusoe.ai/load-balancer-id"
	vmIDFilePath                               = "/sys/class/dmi/id/product_uuid"
	NodeNameFlag                               = "node-name"
	OpSuccess                         opStatus = "SUCCEEDED"
	CrusoeAPIEndpointFlag                      = "crusoe-api-endpoint"
	CrusoeAccessKeyFlag                        = "crusoe-elb-access-key"
	CrusoeSecretKeyFlag                        = "crusoe-elb-secret-key" //nolint:gosec // false positive, this is a flag name
	CrusoeProjectIDFlag                        = "crusoe-project-id"
	CrusoeVPCIDFlag                            = "crusoe-vpc-id"
)

var (
	errInstanceNotFound  = errors.New("instance not found")
	errMultipleInstances = errors.New("multiple instances found")
	errProjectIDNotFound = fmt.Errorf("project ID not found in %s env var or %s node label",
		projectIDEnvKey, projectIDLabelKey)
	errInstanceIDNotFound = fmt.Errorf("instance ID not found in %s env var or %s node label",
		instanceIDEnvKey, instanceIDLabelKey)
	errUnableToGetOpRes = errors.New("failed to get result of operation")
)

// Function to parse health check options from annotations
func ParseHealthCheckOptionsFromAnnotations(annotations map[string]string) *crusoeapi.HealthCheckOptionsExternalLb {
	healthCheckOptions := &crusoeapi.HealthCheckOptionsExternalLb{}

	// Parse each annotation, falling back to default values if not specified
	if failureCount, ok := annotations[AnnotationHealthCheckFailureCount]; ok {
		if parsedFailureCount, err := strconv.Atoi(failureCount); err == nil {
			healthCheckOptions.FailureCount = int64(parsedFailureCount)
		}
	}

	if interval, ok := annotations[AnnotationHealthCheckInterval]; ok {
		if parsedInterval, err := strconv.Atoi(interval); err == nil {
			healthCheckOptions.Interval = int64(parsedInterval)
		}
	}

	if successCount, ok := annotations[AnnotationHealthCheckSuccessCount]; ok {
		if parsedSuccessCount, err := strconv.Atoi(successCount); err == nil {
			healthCheckOptions.SuccessCount = int64(parsedSuccessCount)
		}
	}

	if timeout, ok := annotations[AnnotationHealthCheckTimeout]; ok {
		if parsedTimeout, err := strconv.Atoi(timeout); err == nil {
			healthCheckOptions.Timeout = int64(parsedTimeout)
		}
	}

	// Return nil if no valid annotations were provided
	if healthCheckOptions.FailureCount == 0 &&
		healthCheckOptions.Interval == 0 &&
		healthCheckOptions.SuccessCount == 0 &&
		healthCheckOptions.Timeout == 0 {
		return nil
	}

	return healthCheckOptions
}

func (r *ServiceReconciler) parseListenPortsAndBackends(ctx context.Context, svc *corev1.Service, logger logr.Logger) []crusoeapi.ListenPortAndBackend {
	listenPortsAndBackends := []crusoeapi.ListenPortAndBackend{}

	// Retrieve all nodes to extract their internal IPs
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList); err != nil {
		logger.Error(err, "Failed to list nodes in the cluster")
		return listenPortsAndBackends
	}

	// Extract internal IPs of all nodes
	internalIPs := []string{}
	for _, node := range nodeList.Items {
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				internalIPs = append(internalIPs, address.Address)
			}
		}
	}
	logger.Info("Retrieved internal IPs of nodes", "internalIPs", internalIPs)

	// Map backends using the retrieved internal IPs
	for _, port := range svc.Spec.Ports {
		backends := []crusoeapi.Backend{}

		for _, ip := range internalIPs {
			backend := crusoeapi.Backend{
				Ip:   ip,
				Port: int64(port.TargetPort.IntVal), // Use the TargetPort from service spec
			}
			backends = append(backends, backend)
		}

		// Append to the list of ListenPortAndBackend
		listenPortsAndBackends = append(listenPortsAndBackends, crusoeapi.ListenPortAndBackend{
			ListenPort: int64(port.Port), // Map the port exposed by the service
			Backends:   backends,
		})
	}

	return listenPortsAndBackends
}

//nolint:cyclop // function is already fairly clean
func GetHostInstance(ctx context.Context) (*crusoeapi.InstanceV1Alpha5, *crusoeapi.APIClient, error) {
	logger := log.FromContext(ctx)

	bindErr := utils.BindEnvs()
	if bindErr != nil {
		return nil, nil, fmt.Errorf("could not bind env variables from helm: %w", bindErr)
	}

	logger.Info("Creating Crusoe client with config", "endpoint", viper.GetString(CrusoeAPIEndpointFlag))
	crusoeClient := crusoe.NewCrusoeClient(
		viper.GetString(CrusoeAPIEndpointFlag),
		viper.GetString(CrusoeAccessKeyFlag),
		viper.GetString(CrusoeSecretKeyFlag),
		"crusoe-external-load-balancer-controller/0.0.1",
	)

	var projectID string

	var instanceID string

	projectID = viper.GetString(CrusoeProjectIDFlag)
	if projectID == "" {
		var ok bool
		kubeClientConfig, configErr := rest.InClusterConfig()
		if configErr != nil {
			return nil, nil, fmt.Errorf("could not get kube client config: %w", configErr)
		}

		kubeClient, clientErr := kubernetes.NewForConfig(kubeClientConfig)
		if clientErr != nil {
			return nil, nil, fmt.Errorf("could not get kube client: %w", clientErr)
		}

		hostNode, nodeFetchErr := kubeClient.CoreV1().Nodes().Get(ctx, viper.GetString(NodeNameFlag), metav1.GetOptions{})
		if nodeFetchErr != nil {
			return nil, nil, fmt.Errorf("could not fetch current node with kube client: %w", nodeFetchErr)
		}

		projectID, ok = hostNode.Labels[projectIDLabelKey]
		if !ok {
			return nil, nil, errProjectIDNotFound
		}

		// Note: if missing label check what nodepool image is being used
		instanceID, ok = hostNode.Labels[instanceIDLabelKey]
		if !ok {
			return nil, nil, errInstanceIDNotFound
		}

	}

	instances, _, err := crusoeClient.VMsApi.ListInstances(ctx, projectID,
		&crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString(instanceID),
		})
	if err != nil {
		return nil, crusoeClient, fmt.Errorf("failed to list instances: %w", err)
	}

	if len(instances.Items) == 0 {
		return nil, nil, fmt.Errorf("%w: %s", errInstanceNotFound, instanceID)
	} else if len(instances.Items) > 1 {
		return nil, nil, fmt.Errorf("%w: %s", errMultipleInstances, instanceID)
	}

	return &instances.Items[0], crusoeClient, nil
}

func OpResultToItem[T any](res interface{}) (*T, error) {
	bytes, err := json.Marshal(res)
	if err != nil {
		return nil, errUnableToGetOpRes
	}

	var item T
	err = json.Unmarshal(bytes, &item)
	if err != nil {
		return nil, errUnableToGetOpRes
	}

	return &item, nil
}

// update helper logic below
func (r *ServiceReconciler) updateNodePortService(
	ctx context.Context,
	svc *corev1.Service,
	logger logr.Logger,
) (bool, error) {
	nodePortServiceName := utils.GenerateNodePortServiceName(svc.Name)

	// Attempt to fetch existing NodePort Service
	existingNodePortService := &corev1.Service{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: svc.Namespace,
		Name:      nodePortServiceName,
	}, existingNodePortService)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			// NodePort Service does not exist; create it using handleCreate
			logger.Info("NodePort Service not found; creating a new one", "nodePortService", nodePortServiceName)
			_, createErr := r.handleCreate(ctx, svc)
			return true, createErr
		}
		// Unexpected error
		logger.Error(err, "Failed to fetch NodePort Service", "nodePortService", nodePortServiceName)
		return false, err
	}

	// Track if we need to update the NodePort service
	updated := false

	// Compare and update ports if changed
	if !utils.EqualPorts(existingNodePortService.Spec.Ports, svc.Spec.Ports) {
		logger.Info("Updating NodePort Service ports", "nodePortService", nodePortServiceName)

		// Copy ports from svc, then clear NodePort to avoid collisions
		newPorts := utils.CopyPortsFromService(svc)
		for i := range newPorts {
			newPorts[i].NodePort = 0
		}
		existingNodePortService.Spec.Ports = newPorts
		updated = true
	}

	// Compare and update selectors if changed
	if svc.Spec.Selector != nil && !utils.EqualSelectors(existingNodePortService.Spec.Selector, svc.Spec.Selector) {
		logger.Info("Updating NodePort Service selector", "nodePortService", nodePortServiceName)
		existingNodePortService.Spec.Selector = svc.Spec.Selector
		updated = true
	}

	if updated {
		// Apply updates
		if err := r.Client.Update(ctx, existingNodePortService); err != nil {
			logger.Error(err, "Failed to update NodePort Service", "nodePortService", nodePortServiceName)
			return false, err
		}
		logger.Info("Successfully updated NodePort Service", "nodePortService", nodePortServiceName)
		return true, nil
	}

	// No updates needed
	logger.Info("No updates needed for NodePort Service", "nodePortService", nodePortServiceName)
	return false, nil
}

func (r *ServiceReconciler) updateLoadBalancer(
	ctx context.Context,
	svc *corev1.Service,
	logger logr.Logger,
) error {

	loadBalancerID := svc.Annotations[loadbalancerIDLabelKey]
	listenPortsAndBackends := r.parseListenPortsAndBackends(ctx, svc, logger)
	healthCheckOptions := ParseHealthCheckOptionsFromAnnotations(svc.Annotations)

	// Example: gather fields that might change in the external LB
	lbUpdatePayload := swagger.ExternalLoadBalancerPatchRequest{
		Id: loadBalancerID, // you'd retrieve from annotation or status
		// Possibly gather new health check options or listen ports from svc annotations
		HealthCheckOptions: healthCheckOptions,
		// For listen ports, you might parse from svc.Spec.Ports
		ListenPortsAndBackends: listenPortsAndBackends,
	}

	logger.Info("Updating external load balancer with new specs", "lbID", lbUpdatePayload.Id)
	lb_updated, httpResp, err := r.CrusoeClient.ExternalLoadBalancersApi.UpdateExternalLoadBalancer(ctx, lbUpdatePayload, r.HostInstance.ProjectId, loadBalancerID)
	if err != nil {
		logger.Error(err, "Failed to update load balancer via API")
		return err
	}

	defer func() {
		if err := httpResp.Body.Close(); err != nil {
			logger.Error(err, "Failed to close http response body: %v")
		}
	}()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		logger.Error(nil, "Unexpected response from LB Update API", "status", httpResp.StatusCode)
		return fmt.Errorf("unexpected status from LB API: %d", httpResp.StatusCode)
	}
	logger.Info("Successfully updated external load balancer", "LB", lb_updated)

	return nil
}
