package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"lb_controller/internal/crusoe"
	utils "lb_controller/internal/utils"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/go-logr/logr"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
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
	ManageFirewallRuleKey                      = "crusoe.ai/manage-firewall-rule"
	FirewallRuleOperationIdKey                 = "crusoe.ai/firewall-rule-operation-id"
	FirewallRuleIdKey                          = "crusoe.ai/firewall-rule-id"
	NodeNameFlag                               = "node-name"
	OpSuccess                         opStatus = "SUCCEEDED"
	OpFailed                          opStatus = "FAILED"
	CrusoeAPIEndpointFlag                      = "crusoe-api-endpoint"
	CrusoeAccessKeyFlag                        = "crusoe-elb-access-key"
	CrusoeSecretKeyFlag                        = "crusoe-elb-secret-key" //nolint:gosec // false positive, this is a flag name
	CrusoeProjectIDFlag                        = "crusoe-project-id"
	CrusoeVPCIDFlag                            = "crusoe-vpc-id"
	CrusoeSubnetIDFlag                         = "crusoe-subnet-id"
)

// Protocols accepted by the Crusoe external load balancer API.
//
// A load balancer carries a single protocol that applies to all of its listen ports;
// the protocol is set at creation time and cannot be changed afterwards, because the
// update API carries no protocol field.
const (
	LoadBalancerProtocolTCP = "LOAD_BALANCER_PROTOCOL_TCP"
	LoadBalancerProtocolUDP = "LOAD_BALANCER_PROTOCOL_UDP"

	// AnnotationLoadBalancerProtocol records the protocol a Service's load balancer was
	// created with, so later reconciles can detect drift without an extra API call.
	// Load balancers created before UDP support do not carry it, and are left alone.
	AnnotationLoadBalancerProtocol = "crusoe.ai/load-balancer-protocol"
)

var (
	errUnableToGetOpRes        = errors.New("failed to get result of operation")
	errMixedPortProtocols      = errors.New("service mixes TCP and UDP ports; one protocol per load balancer")
	errUnsupportedPortProtocol = errors.New("unsupported service port protocol")
)

type firewallRuleArgs struct {
	projectID        string
	vpcID            string
	ruleName         string
	destinationPorts []string
	protocols        []string
	sources          []swagger.FirewallRuleObject
}

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

// effectivePortProtocol returns the protocol of a Service port, treating an unset
// protocol as TCP. Kubernetes defaults spec.ports[].protocol to TCP when it is
// omitted, so this only matters for Service objects built in-process (tests) or
// read from a source that did not go through defaulting.
func effectivePortProtocol(port corev1.ServicePort) corev1.Protocol {
	if port.Protocol == "" {
		return corev1.ProtocolTCP
	}

	return port.Protocol
}

// DeriveLoadBalancerProtocol determines the Crusoe external load balancer protocol
// required by a Service.
//
// A load balancer serves exactly one protocol across all of its listen ports, so a
// Service that mixes TCP and UDP ports cannot be served by a single load balancer and
// is rejected. Such a Service should be split into one Service per protocol.
//
// Every Service that was servable before UDP support resolves to TCP here: explicit
// TCP ports, ports with no protocol set, and a Service with no ports at all.
func DeriveLoadBalancerProtocol(svc *corev1.Service) (string, error) {
	var hasTCP, hasUDP bool

	for _, port := range svc.Spec.Ports {
		switch protocol := effectivePortProtocol(port); protocol {
		case corev1.ProtocolTCP:
			hasTCP = true
		case corev1.ProtocolUDP:
			hasUDP = true
		default:
			// SCTP is a valid Kubernetes port protocol but is not offered by Crusoe
			// external load balancers. Reject it rather than silently building a TCP
			// load balancer in front of an SCTP node port.
			return "", fmt.Errorf("%w: port %d uses %s", errUnsupportedPortProtocol, port.Port, protocol)
		}
	}

	if hasTCP && hasUDP {
		return "", errMixedPortProtocols
	}

	if hasUDP {
		return LoadBalancerProtocolUDP, nil
	}

	return LoadBalancerProtocolTCP, nil
}

// servicePortsForProtocol returns the Service ports that belong on a load balancer of
// the given protocol. Any protocol other than UDP is treated as TCP, so an unknown or
// empty load balancer protocol keeps the historical TCP behaviour.
func servicePortsForProtocol(ports []corev1.ServicePort, lbProtocol string) []corev1.ServicePort {
	wanted := corev1.ProtocolTCP
	if lbProtocol == LoadBalancerProtocolUDP {
		wanted = corev1.ProtocolUDP
	}

	matching := make([]corev1.ServicePort, 0, len(ports))
	for _, port := range ports {
		if effectivePortProtocol(port) == wanted {
			matching = append(matching, port)
		}
	}

	return matching
}

func (r *ServiceReconciler) parseListenPortsAndBackends(ctx context.Context, ports []corev1.ServicePort, logger logr.Logger) []crusoeapi.ListenPortAndBackend {
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
	for _, port := range ports {
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

// isKnownLoadBalancerProtocol reports whether a recorded protocol is one this controller
// wrote. The annotation lives on the Service and is therefore user-writable, so a value
// it does not recognise is treated as absent rather than trusted.
func isKnownLoadBalancerProtocol(protocol string) bool {
	return protocol == LoadBalancerProtocolTCP || protocol == LoadBalancerProtocolUDP
}

// warnOnProtocolDrift reports, without failing the reconcile, that a Service's ports no
// longer agree with the protocol its load balancer was created with. The protocol of an
// external load balancer is fixed at creation time, so the mismatch can only be resolved
// by recreating the Service. Listeners for the load balancer's own protocol keep being
// reconciled in the meantime.
//
// Logged at Info rather than Error on purpose: the condition is a Service misconfiguration
// that no retry can clear, and every Node event re-reconciles every LoadBalancer Service,
// so logging it at Error would emit a permanent error line per Service per node event.
func warnOnProtocolDrift(svc *corev1.Service, lbProtocol string, logger logr.Logger) {
	desired, err := DeriveLoadBalancerProtocol(svc)
	if err != nil {
		logger.Info("Service ports can no longer be served by its load balancer",
			"service", svc.Name, "namespace", svc.Namespace,
			"loadBalancerProtocol", lbProtocol, "reason", err.Error())

		return
	}

	if desired != lbProtocol {
		logger.Info("Load balancer protocol cannot be changed after creation, recreate the Service to change it",
			"service", svc.Name, "namespace", svc.Namespace,
			"loadBalancerProtocol", lbProtocol, "servicePortProtocol", desired)
	}
}

func (r *ServiceReconciler) updateLoadBalancer(
	ctx context.Context,
	svc *corev1.Service,
	logger logr.Logger,
) error {

	loadBalancerID := svc.Annotations[loadbalancerIDLabelKey]

	// Load balancers created before UDP support carry no protocol annotation, and an
	// annotation holding an unrecognised value is ignored. Both fall back to offering
	// every port as a listener, exactly as this controller behaved before UDP support.
	ports := svc.Spec.Ports
	if lbProtocol := svc.Annotations[AnnotationLoadBalancerProtocol]; isKnownLoadBalancerProtocol(lbProtocol) {
		warnOnProtocolDrift(svc, lbProtocol, logger)

		ports = servicePortsForProtocol(svc.Spec.Ports, lbProtocol)
		if len(ports) != len(svc.Spec.Ports) {
			logger.Info("Ignoring Service ports that do not match the load balancer protocol",
				"service", svc.Name, "namespace", svc.Namespace, "loadBalancerProtocol", lbProtocol,
				"matchingPorts", len(ports), "totalPorts", len(svc.Spec.Ports))
		}

		// Every port has been edited away from the load balancer's protocol. The update
		// API rejects an empty listener list, so sending one would fail on every reconcile;
		// worse, an API version that accepted it would strip a live load balancer. Leave
		// the load balancer as it is until the Service is recreated.
		if len(ports) == 0 && len(svc.Spec.Ports) > 0 {
			logger.Info("No Service ports match the load balancer protocol, leaving the load balancer unchanged",
				"service", svc.Name, "namespace", svc.Namespace, "loadBalancerProtocol", lbProtocol)

			return nil
		}
	}

	listenPortsAndBackends := r.parseListenPortsAndBackends(ctx, ports, logger)
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

// isSubnetIDProvided checks if the subnet ID environment variable is provided
func isSubnetIDProvided() bool {
	subnetID := viper.GetString(CrusoeSubnetIDFlag)
	logger := log.FromContext(context.Background())
	logger.Info("Checking subnet ID", "subnetID", subnetID, "isEmpty", subnetID == "")
	return subnetID != ""
}

// getVPCInfoForSelfManagedCluster gets subnet ID from environment variable and fetches VPC info from Crusoe API
func getVPCInfoForSelfManagedCluster(ctx context.Context, crusoeClient *crusoeapi.APIClient) (vpcID, subnetID, location string, err error) {
	subnetID = viper.GetString(CrusoeSubnetIDFlag)
	if subnetID == "" {
		return "", "", "", fmt.Errorf("CRUSOE_SUBNET_ID environment variable must be defined in helm values if used")
	}

	// Fetch VPC and location information from the subnet using Crusoe API
	projectId := viper.GetString(CrusoeProjectIDFlag)
	subnet, _, err := crusoeClient.VPCSubnetsApi.GetVPCSubnet(ctx, projectId, subnetID)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to get subnet information from Crusoe API: %w", err)
	}

	vpcID = subnet.VpcNetworkId
	location = subnet.Location

	return vpcID, subnetID, location, nil
}

// getVPCAndLocationInfo determines VPC ID and location based on cluster type (self-managed vs Crusoe-managed)
func getVPCAndLocationInfo(ctx context.Context, crusoeClient *crusoeapi.APIClient, logger logr.Logger) (vpcID, location string, err error) {
	logger.Info("Starting getVPCAndLocationInfo")

	if isSubnetIDProvided() {
		logger.Info("Subnet ID provided, using self-managed cluster path")
		var subnetID string
		vpcID, subnetID, location, err = getVPCInfoForSelfManagedCluster(ctx, crusoeClient)
		if err != nil {
			logger.Error(err, "Failed to get VPC information from subnet")
			return "", "", err
		}

		logger.Info("Retrieved VPC info from subnet", "vpcID", vpcID, "subnetID", subnetID, "location", location)

		return vpcID, location, nil
	}
	// Use Crusoe cluster information
	logger.Info("No subnet ID provided, using Crusoe-managed cluster configuration")
	projectId := viper.GetString(CrusoeProjectIDFlag)
	cluster, _, err := crusoeClient.KubernetesClustersApi.GetCluster(ctx, projectId, viper.GetString(CrusoeClusterIDFlag))
	if err != nil {
		logger.Error(err, "Failed to get cluster", "clusterID", viper.GetString(CrusoeClusterIDFlag))
		return "", "", err
	}
	subnet, _, err := crusoeClient.VPCSubnetsApi.GetVPCSubnet(ctx, projectId, cluster.SubnetId)
	if err != nil {
		logger.Error(err, "Failed to get vpc network id from cluster subnet id ", "subnetID", cluster.SubnetId)
		return "", "", err
	}
	vpcID = subnet.VpcNetworkId
	location = subnet.Location

	return vpcID, location, err
}

func (r *ServiceReconciler) ensureFirewallRule(ctx context.Context, svc *corev1.Service) error {
	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}
	svcCopy := svc.DeepCopy()

	if val, exists := svc.Annotations[ManageFirewallRuleKey]; !exists || val != "true" {
		return nil
	}

	logger := log.FromContext(ctx)

	args := r.GetFirewallRuleArgs(ctx, svc)
	if args == nil {
		// skipping firewall rule creation based on args
		return nil
	}

	if _, exists := svc.Annotations[FirewallRuleIdKey]; exists {
		rule, httpResp, err := r.CrusoeClient.VPCFirewallRulesApi.GetVPCFirewallRule(ctx, args.projectID, svc.Annotations[FirewallRuleIdKey])
		if err != nil {
			if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
				logger.Info("Firewall rule not found, recreating")
				delete(svc.Annotations, FirewallRuleIdKey)
				delete(svc.Annotations, FirewallRuleOperationIdKey)
			} else {
				logger.Error(err, "Failed to get firewall rule")
				return err
			}
		} else {
			matches := CompareFirewallRule(ctx, &rule, args)
			if matches {
				logger.Info("Firewall rule matches service, no action needed")
				return nil
			}
			logger.Info("Firewall rule does not match service, patching")
			_, _, err := r.CrusoeClient.VPCFirewallRulesApi.PatchVPCFirewallRule(ctx, swagger.VpcFirewallRulesPatchRequest{
				Name: args.ruleName,
				Destinations: []swagger.FirewallRuleObject{
					{ResourceId: args.vpcID},
				},
				DestinationPorts: args.destinationPorts,
				Protocols:        args.protocols,
				Sources:          args.sources,
			}, args.projectID, svc.Annotations[FirewallRuleIdKey])

			if err != nil {
				logger.Error(err, "Failed to patch firewall rule")
				return err
			}

			return nil
		}
	} else if operationID, exists := svc.Annotations[FirewallRuleOperationIdKey]; exists {
		logger.Info("Firewall rule operation exists, checking status")

		op, firewallRule, err := GetFirewallRuleOperationResult(ctx, r.CrusoeClient, operationID)
		if err != nil {
			logger.Error(err, "Failed to get firewall rule operation result")

			return err
		}

		if op == nil {
			logger.Error(errUnableToGetOpRes, "Firewall rule operation exists but could nt be retrieved", "operationID", operationID)

			return errUnableToGetOpRes
		}

		if op.State == string(OpFailed) {
			marshalledResult, _ := json.Marshal(op.Result) // ignore error or handle it
			resultText := strings.ToLower(string(marshalledResult))
			if strings.Contains(resultText, "already exists") {
				logger.Info("Firewall rule already exists")
				rules, _, err := r.CrusoeClient.VPCFirewallRulesApi.ListVPCFirewallRules(ctx, args.projectID)
				if err != nil {
					logger.Error(err, "Failed to get firewall rules")
					return err
				}
				for _, rule := range rules.Items {
					if rule.Name == args.ruleName {
						svc.Annotations[FirewallRuleIdKey] = rule.Id
					}
				}
				return r.Patch(ctx, svc, client.MergeFrom(svcCopy))
			} else {
				logger.Info("Firewall rule operation failed, retrying", "result", op.Result)
				delete(svc.Annotations, FirewallRuleOperationIdKey)
			}
		} else if op.State == string(OpSuccess) {
			logger.Info("Firewall rule operation complete", "ruleInfo", firewallRule)
			svc.Annotations[FirewallRuleIdKey] = firewallRule.Id

			return r.Patch(ctx, svc, client.MergeFrom(svcCopy))
		} else {
			logger.Info("Firewall rule operation in progress")

			return nil
		}
	}

	logger.Info("Creating firewall rule", "name", args.ruleName, "vpcNetworkId", args.vpcID, "destinationPorts", args.destinationPorts, "protocols", args.protocols)
	op_resp, _, err := r.CrusoeClient.VPCFirewallRulesApi.CreateVPCFirewallRule(ctx,
		swagger.VpcFirewallRulesPostRequestV1Alpha5{
			Name:   args.ruleName,
			Action: "ALLOW",
			Destinations: []swagger.FirewallRuleObject{
				{ResourceId: args.vpcID},
			},
			DestinationPorts: args.destinationPorts,
			Protocols:        args.protocols,
			VpcNetworkId:     args.vpcID,
			Direction:        "INGRESS",
			Sources:          args.sources,
		}, args.projectID)
	if err != nil {
		logger.Error(err, "Failed to create firewall rule", "name", args.ruleName)
		return err
	}

	svc.Annotations[FirewallRuleOperationIdKey] = op_resp.Operation.OperationId
	logger.Info("Created firewall rule operation", "name", args.ruleName, "operationId", op_resp.Operation.OperationId)
	return r.Patch(ctx, svc, client.MergeFrom(svcCopy))
}

func (r *ServiceReconciler) deleteFirewallRule(ctx context.Context, svc *corev1.Service) error {
	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}

	if val, exists := svc.Annotations[ManageFirewallRuleKey]; !exists || val != "true" {
		return nil
	}

	logger := log.FromContext(ctx)

	projectId := viper.GetString(CrusoeProjectIDFlag)
	if projectId == "" {
		return fmt.Errorf("project ID is required")
	}

	var ruleID string
	if existingRuleID, exists := svc.Annotations[FirewallRuleIdKey]; !exists || existingRuleID == "" {
		if operationID, exists := svc.Annotations[FirewallRuleOperationIdKey]; !exists {
			logger.Info("Firewall rule operation ID not found, skipping deletion", "service", svc.Name)
			return nil
		} else {
			_, firewallRule, err := GetFirewallRuleOperationResult(ctx, r.CrusoeClient, operationID)
			if err != nil || firewallRule == nil {
				logger.Error(err, "Firewall rule not found or operation failed")
				return nil
			}
			ruleID = firewallRule.Id
		}
	} else {
		ruleID = existingRuleID
	}

	_, httpResp, err := r.CrusoeClient.VPCFirewallRulesApi.DeleteVPCFirewallRule(ctx, projectId, ruleID)
	if err != nil {
		if httpResp != nil && httpResp.StatusCode == http.StatusNotFound {
			logger.Info("Firewall rule not found (404), assuming already deleted", "ruleID", ruleID)

			svcCopy := svc.DeepCopy()
			delete(svc.Annotations, FirewallRuleIdKey)
			delete(svc.Annotations, FirewallRuleOperationIdKey)
			return r.Patch(ctx, svc, client.MergeFrom(svcCopy))
		}
		logger.Error(err, "Failed to delete firewall rule", "ruleID", ruleID)
		return err
	}

	logger.Info("Deleted firewall rule", "ruleID", ruleID)

	svcCopy := svc.DeepCopy()
	delete(svc.Annotations, FirewallRuleIdKey)
	delete(svc.Annotations, FirewallRuleOperationIdKey)
	return r.Patch(ctx, svc, client.MergeFrom(svcCopy))
}

func (r *ServiceReconciler) GetFirewallRuleArgs(ctx context.Context, svc *corev1.Service) *firewallRuleArgs {
	logger := log.FromContext(ctx)
	projectID := viper.GetString(CrusoeProjectIDFlag)
	if projectID == "" {
		logger.Info("CRUSOE_PROJECT_ID not set, skipping firewall rule creation")
		return nil
	}

	vpcID, _, err := getVPCAndLocationInfo(ctx, r.CrusoeClient, logger)
	if err != nil {
		logger.Error(err, "Failed to get VPC info, skipping firewall rule creation")
		return nil
	}

	ruleName, err := GenerateLoadBalancerResourceName(svc.Namespace, svc.Name, string(svc.UID))
	if err != nil {
		logger.Error(err, "Failed to generate firewall rule name, skipping",
			"service", svc.Name, "namespace", svc.Namespace)
		return nil
	}

	sources := []swagger.FirewallRuleObject{{Cidr: "0.0.0.0/0"}}
	if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
		sources = make([]swagger.FirewallRuleObject, len(svc.Spec.LoadBalancerSourceRanges))
		for i, source := range svc.Spec.LoadBalancerSourceRanges {
			sources[i] = swagger.FirewallRuleObject{Cidr: source}
		}
	}

	protocols := []string{}
	for _, port := range svc.Spec.Ports {
		if port.Protocol != "" {
			if !slices.Contains(protocols, strings.ToLower(string(port.Protocol))) && string(port.Protocol) != "" {
				protocols = append(protocols, strings.ToLower(string(port.Protocol)))
			}
		} else if !slices.Contains(protocols, "tcp") {
			protocols = append(protocols, "tcp")
		}
	}

	destinationPorts := []string{}
	// Deliberately unfiltered: the firewall rule covers every node port the Service
	// exposes, and its protocol list is derived from the ports directly just above.
	listenPortsAndBackends := r.parseListenPortsAndBackends(ctx, svc.Spec.Ports, logger)
	for _, portAndBackend := range listenPortsAndBackends {
		for _, backend := range portAndBackend.Backends {
			if !slices.Contains(destinationPorts, fmt.Sprintf("%d", backend.Port)) {
				destinationPorts = append(destinationPorts, fmt.Sprintf("%d", backend.Port))
			}
		}
	}
	if len(destinationPorts) == 0 {
		// TODO: add to service
		logger.Info("No backends found, skipping firewall rule creation")
		return nil
	}

	return &firewallRuleArgs{
		projectID:        projectID,
		vpcID:            vpcID,
		ruleName:         ruleName,
		destinationPorts: destinationPorts,
		protocols:        protocols,
		sources:          sources,
	}
}

func CompareFirewallRule(ctx context.Context, rule *swagger.VpcFirewallRule, args *firewallRuleArgs) bool {
	logger := log.FromContext(ctx)
	sort.Strings(rule.DestinationPorts)
	sort.Strings(args.destinationPorts)
	sort.Slice(rule.Sources, func(i, j int) bool {
		return rule.Sources[i].Cidr < rule.Sources[j].Cidr
	})
	sort.Slice(args.sources, func(i, j int) bool {
		return args.sources[i].Cidr < args.sources[j].Cidr
	})
	sort.Strings(rule.Protocols)
	sort.Strings(args.protocols)

	if rule.Name != args.ruleName {
		logger.Info("Rule name does not match", "ruleName", rule.Name, "argsRuleName", args.ruleName)
	}
	if !slices.Equal(rule.Protocols, args.protocols) {
		logger.Info("Rule protocols do not match", "ruleProtocols", rule.Protocols, "argsProtocols", args.protocols)
	}
	if rule.VpcNetworkId != args.vpcID {
		logger.Info("Rule VPC networks do not match", "ruleVPC", rule.VpcNetworkId, "argsVPC", args.vpcID)
	}
	if !slices.Equal(rule.Sources, args.sources) {
		logger.Info("Rule sources do not match", "ruleSources", rule.Sources, "argsSources", args.sources)
	}
	if !slices.Equal(rule.DestinationPorts, args.destinationPorts) {
		logger.Info("Rule destination ports do not match", "ruleDestinationPorts", rule.DestinationPorts, "argsDestinationPorts", args.destinationPorts)
	}

	return rule.Name == args.ruleName && rule.VpcNetworkId == args.vpcID &&
		slices.Equal(rule.DestinationPorts, args.destinationPorts) && slices.Equal(rule.Protocols, args.protocols) &&
		slices.Equal(rule.Sources, args.sources)
}

func GetFirewallRuleOperationResult(ctx context.Context, crusoeClient *crusoeapi.APIClient, operationID string) (
	*swagger.Operation, *swagger.VpcFirewallRule, error,
) {
	op, httpResp, err := crusoeClient.VPCFirewallRuleOperationsApi.GetNetworkingVPCFirewallRulesOperation(
		ctx, viper.GetString(utils.CrusoeProjectIDFlag), operationID,
	)
	if err != nil {
		return nil, nil, err
	}
	if httpResp.StatusCode != http.StatusOK {
		return &op, nil, fmt.Errorf("failed to get firewall rule operation: %s", httpResp.Status)
	}

	firewallRule, err := OpResultToItem[swagger.VpcFirewallRule](op.Result)
	if err != nil {
		return &op, nil, err
	}
	return &op, firewallRule, nil
}

// GenerateLoadBalancerResourceName creates a unique, deterministic name shared by
// a load balancer and its associated firewall rule
// Format: <namespace>-<service-name>-<8-char-hash>
// Hash is derived from the Kubernetes service UID to ensure global uniqueness
// Note: Kubernetes service and namespace names are already DNS-1123 compliant (lowercase alphanumeric, '-', '.')
func GenerateLoadBalancerResourceName(namespace, serviceName, serviceUID string) (string, error) {
	// Generate hash from service UID
	// Service UIDs are globally unique across all clusters
	hash := utils.GenerateServiceHash(serviceUID)

	// Construct name
	name := fmt.Sprintf("%s-%s-%s", namespace, serviceName, hash)

	// Validate length (must be < 50 chars)
	if len(name) >= 50 {
		// Truncate service name to fit within limit
		maxServiceNameLen := 49 - len(namespace) - len(hash) - 2 // 2 for hyphens, 49 to ensure < 50
		if maxServiceNameLen < 5 {
			return "", fmt.Errorf("namespace '%s' is too long to create valid LB name", namespace)
		}
		truncatedServiceName := serviceName
		if len(serviceName) > maxServiceNameLen {
			truncatedServiceName = serviceName[:maxServiceNameLen]
		}
		name = fmt.Sprintf("%s-%s-%s", namespace, truncatedServiceName, hash)
	}

	return name, nil
}
