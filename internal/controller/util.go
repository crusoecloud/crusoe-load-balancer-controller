package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lb_controller/internal/crusoe"
	utils "lb_controller/internal/utils"
	"strconv"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/go-logr/logr"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type opStatus string

const (
	AnnotationHealthCheckFailureCount = "crusoe.ai/health-check-failure-count"
	AnnotationHealthCheckInterval     = "crusoe.ai/health-check-interval"
	AnnotationHealthCheckSuccessCount = "crusoe.ai/health-check-success-count"
	AnnotationHealthCheckTimeout      = "crusoe.ai/health-check-timeout"
	AnnotationCrusoeManagedCluster    = "crusoe.ai/crusoe-managed-cluster"
	// Self-managed cluster configuration with organized prefixes
	AnnotationSelfManagedVPCID             = "crusoe.ai/self-managed.vpc-id"
	AnnotationSelfManagedSubnetID          = "crusoe.ai/self-managed.subnet-id"
	AnnotationSelfManagedLocation          = "crusoe.ai/self-managed.location"
	projectIDEnvKey                        = "CRUSOE_PROJECT_ID"
	projectIDLabelKey                      = "crusoe.ai/project.id"
	instanceIDEnvKey                       = "CRUSOE_INSTANCE_ID"
	instanceIDLabelKey                     = "crusoe.ai/instance.id"
	loadbalancerIDLabelKey                 = "crusoe.ai/load-balancer-id"
	vmIDFilePath                           = "/sys/class/dmi/id/product_uuid"
	NodeNameFlag                           = "node-name"
	OpSuccess                     opStatus = "SUCCEEDED"
	CrusoeAPIEndpointFlag                  = "crusoe-api-endpoint"
	CrusoeAccessKeyFlag                    = "crusoe-elb-access-key"
	CrusoeSecretKeyFlag                    = "crusoe-elb-secret-key" //nolint:gosec // false positive, this is a flag name
	CrusoeProjectIDFlag                    = "crusoe-project-id"
	CrusoeVPCIDFlag                        = "crusoe-vpc-id"
)

var (
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

func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ServiceReconciler) parseListenPortsAndBackends(ctx context.Context, svc *corev1.Service, logger logr.Logger) []crusoeapi.ListenPortAndBackend {
	listenPortsAndBackends := []crusoeapi.ListenPortAndBackend{}

	// Retrieve all nodes to extract their internal IPs
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList); err != nil {
		logger.Error(err, "Failed to list nodes in the cluster")
		return listenPortsAndBackends
	}

	// // Filter out only Ready nodes
	var readyNodes []corev1.Node
	for _, node := range nodeList.Items {
		if isNodeReady(&node) {
			readyNodes = append(readyNodes, node)
		}
	}

	// Extract internal IPs of all nodes
	internalIPs := []string{}
	for _, node := range readyNodes {
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				internalIPs = append(internalIPs, address.Address)
			}
		}
	}
	logger.Info("Retrieved internal IPs of nodes", "internalIPs", internalIPs)

	// For each service port, create a mapping to its corresponding node port and backends
	for _, port := range svc.Spec.Ports {
		// Skip if no node port is assigned
		if port.NodePort == 0 {
			logger.Info("Skipping port with no node port assigned", "port", port.Port)
			continue
		}

		// Create backends for this specific port
		var backends []crusoeapi.Backend
		for _, ip := range internalIPs {
			backends = append(backends, crusoeapi.Backend{
				Ip:   ip,
				Port: int64(port.NodePort),
			})
		}

		logger.Info("Mapped service port to node port",
			"servicePort", port.Port,
			"nodePort", port.NodePort,
			"protocol", port.Protocol,
			"backends", backends)

		// Add to the list of ListenPortAndBackend
		listenPortsAndBackends = append(listenPortsAndBackends, crusoeapi.ListenPortAndBackend{
			ListenPort: int64(port.Port), // The port exposed by the service
			Backends:   backends,
		})
	}

	return listenPortsAndBackends
}

//nolint:cyclop // function is already fairly clean
func GetCrusoeClient(ctx context.Context) (*crusoeapi.APIClient, error) {
	logger := log.FromContext(ctx)

	bindErr := utils.BindEnvs()
	if bindErr != nil {
		return nil, fmt.Errorf("could not bind env variables from helm: %w", bindErr)
	}

	logger.Info("Creating Crusoe client with config", "endpoint", viper.GetString(CrusoeAPIEndpointFlag))
	crusoeClient := crusoe.NewCrusoeClient(
		viper.GetString(CrusoeAPIEndpointFlag),
		viper.GetString(CrusoeAccessKeyFlag),
		viper.GetString(CrusoeSecretKeyFlag),
		"crusoe-external-load-balancer-controller/0.0.1",
	)

	return crusoeClient, nil
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
	projectId := viper.GetString(CrusoeProjectIDFlag)
	lb_updated, httpResp, err := r.CrusoeClient.ExternalLoadBalancersApi.UpdateExternalLoadBalancer(ctx, lbUpdatePayload, projectId, loadBalancerID)
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

// isCrusoeManagedCluster checks if the service is running on a Crusoe-managed cluster
func isCrusoeManagedCluster(svc *corev1.Service) bool {
	if svc.Annotations == nil {
		return true // Default to Crusoe-managed for backward compatibility
	}

	// Check if the annotation explicitly sets it to false
	if managed, exists := svc.Annotations[AnnotationCrusoeManagedCluster]; exists {
		return managed != "false"
	}

	return true // Default to Crusoe-managed for backward compatibility
}

// getVPCInfoForSelfManagedCluster extracts VPC information from service annotations
func getVPCInfoForSelfManagedCluster(svc *corev1.Service) (vpcID, subnetID, location string, err error) {
	if svc.Annotations == nil {
		return "", "", "", fmt.Errorf("no annotations found on service")
	}

	vpcID = svc.Annotations[AnnotationSelfManagedVPCID]
	if vpcID == "" {
		return "", "", "", fmt.Errorf("vpc-id annotation is required for self-managed clusters")
	}

	subnetID = svc.Annotations[AnnotationSelfManagedSubnetID]
	if subnetID == "" {
		return "", "", "", fmt.Errorf("subnet-id annotation is required for self-managed clusters")
	}

	location = svc.Annotations[AnnotationSelfManagedLocation]
	if location == "" {
		return "", "", "", fmt.Errorf("location annotation is required for self-managed clusters")
	}

	return vpcID, subnetID, location, nil
}

// Alternative: Get VPC info from JSON structure in single annotation
func getVPCInfoFromJSONAnnotation(svc *corev1.Service) (vpcID, subnetID, location string, err error) {
	if svc.Annotations == nil {
		return "", "", "", fmt.Errorf("no annotations found on service")
	}

	configJSON := svc.Annotations["crusoe.ai/self-managed-config"]
	if configJSON == "" {
		return "", "", "", fmt.Errorf("self-managed-config annotation is required for self-managed clusters")
	}

	var config struct {
		VPCID    string `json:"vpc-id"`
		SubnetID string `json:"subnet-id"`
		Location string `json:"location"`
	}

	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return "", "", "", fmt.Errorf("failed to parse self-managed-config JSON: %w", err)
	}

	if config.VPCID == "" {
		return "", "", "", fmt.Errorf("vpc-id is required in self-managed-config")
	}
	if config.SubnetID == "" {
		return "", "", "", fmt.Errorf("subnet-id is required in self-managed-config")
	}
	if config.Location == "" {
		return "", "", "", fmt.Errorf("location is required in self-managed-config")
	}

	return config.VPCID, config.SubnetID, config.Location, nil
}
