package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lb_controller/internal/crusoe"
	"strconv"

	"github.com/antihax/optional"
	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/go-logr/logr"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	AnnotationHealthCheckFailureCount = "crusoe.ai/health-check-failure-count"
	AnnotationHealthCheckInterval     = "crusoe.ai/health-check-interval"
	AnnotationHealthCheckSuccessCount = "crusoe.ai/health-check-success-count"
	AnnotationHealthCheckTimeout      = "crusoe.ai/health-check-timeout"
	projectIDEnvKey                   = "CRUSOE_PROJECT_ID"
	projectIDLabelKey                 = "crusoe.ai/project.id"
	instanceIDEnvKey                  = "CRUSOE_INSTANCE_ID"
	instanceIDLabelKey                = "crusoe.ai/instance.id"
	vmIDFilePath                      = "/sys/class/dmi/id/product_uuid"
	CrusoeProjectIDFlag               = "crusoe-project-id"
	NodeNameFlag                      = "node-name"
)

var (
	errInstanceNotFound  = errors.New("instance not found")
	errMultipleInstances = errors.New("multiple instances found")
	errVMIDReadFailed    = fmt.Errorf("failed to read %s for VM ID", vmIDFilePath)
	errVMIDParseFailed   = fmt.Errorf("failed to parse %s for VM ID", vmIDFilePath)
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

// TODO should need to call corev1 endpoints should just be able to parse from svc.Spec.Ports
func (r *ServiceReconciler) parseListenPortsAndBackends(ctx context.Context, svc *corev1.Service, logger logr.Logger) []crusoeapi.ListenPortAndBackend {
	listenPortsAndBackends := []crusoeapi.ListenPortAndBackend{}

	for _, port := range svc.Spec.Ports {
		backends := []crusoeapi.Backend{}

		// Retrieve endpoints for the service
		endpointList := &corev1.Endpoints{}
		err := r.Client.Get(ctx, client.ObjectKey{
			Namespace: svc.Namespace,
			Name:      svc.Name,
		}, endpointList)
		if err != nil {
			logger.Error(err, "Failed to get endpoints for Service", "service", svc.Name)
			continue // Skip this port if endpoints are not found
		}

		// TODO: will need to call API to get all endpoints i.e. all the nodepools in the cluster
		// Map backends dynamically from endpoints
		for _, subset := range endpointList.Subsets {
			for _, address := range subset.Addresses {
				backend := crusoeapi.Backend{
					Ip:   address.IP,
					Port: int64(port.TargetPort.IntVal), // Use the TargetPort from service spec
				}
				backends = append(backends, backend)
			}
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
	// crusoeClient := crusoe.NewCrusoeClient(
	// 	"https://api.crusoecloud.site/v1alpha5", //viper.GetString(CrusoeAPIEndpointFlag),
	// 	"tvwt_ao7SeOJBAYoIXgV7Q",               // viper.GetString(CrusoeAccessKeyFlag),
	// 	"9M6kyFCNLmYZNZNJd958hw",               //viper.GetString(CrusoeSecretKeyFlag),
	// 	"crusoe-external-load-balancer-controller/0.0.1",
	// )

	crusoeClient := crusoe.NewCrusoeClient(
		"https://api.crusoecloud.site/v1alpha5", //viper.GetString(CrusoeAPIEndpointFlag),
		"jiWzrrMsSam45JTZjJs_OA",                // viper.GetString(CrusoeAccessKeyFlag),
		"2oYPidrrSaO-d-PBuNrktA",                //viper.GetString(CrusoeSecretKeyFlag),
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
		// TODO: replace np-334f5c73-1.us-east1-a.compute.internal (prod) with viper.GetString(NodeNameFlag)
		hostNode, nodeFetchErr := kubeClient.CoreV1().Nodes().Get(ctx, "np-6e17233f-1.us-eaststaging1-a.compute.internal", metav1.GetOptions{})
		if nodeFetchErr != nil {
			return nil, nil, fmt.Errorf("could not fetch current node with kube client: %w", nodeFetchErr)
		}

		projectID, ok = hostNode.Labels[projectIDLabelKey]
		if !ok {
			return nil, nil, errProjectIDNotFound
		}

		// Note: if missing label check what nodepool image is being used
		// instanceID, ok = hostNode.Labels[instanceIDLabelKey]
		// if !ok {
		// 	return nil, nil, errInstanceIDNotFound
		// }

		instanceID = "87b97c13-e26e-4162-bc1f-b38d5519d375"

	}

	instances, _, err := crusoeClient.VMsApi.ListInstances(ctx, projectID,
		&crusoeapi.VMsApiListInstancesOpts{
			Ids: optional.NewString(instanceID),
		})
	if err != nil {
		return nil, crusoeClient, fmt.Errorf("failed to list instances: %w", err)
	}

	logger.Error(err, "successfully called vms api")

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
