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
	"time"

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
	AnnotationHealthCheckFailureCount = "crusoe.ai/health-check-failure-count"
	AnnotationHealthCheckInterval     = "crusoe.ai/health-check-interval"
	AnnotationHealthCheckSuccessCount = "crusoe.ai/health-check-success-count"
	AnnotationHealthCheckTimeout      = "crusoe.ai/health-check-timeout"
	projectIDEnvKey                   = "CRUSOE_PROJECT_ID"
	projectIDLabelKey                 = "crusoe.ai/project.id"
	instanceIDEnvKey                  = "CRUSOE_INSTANCE_ID"
	instanceIDLabelKey                = "crusoe.ai/instance.id"
	loadbalancerIDLabelKey            = "crusoe.ai/load-balancer-id"
	ManageFirewallRuleKey             = "crusoe.ai/manage-firewall-rule"
	// ManageFirewallRuleEnabled is the ManageFirewallRuleKey value that opts a Service into
	// firewall rule management.
	ManageFirewallRuleEnabled           = "true"
	FirewallRuleOperationIdKey          = "crusoe.ai/firewall-rule-operation-id"
	FirewallRuleIdKey                   = "crusoe.ai/firewall-rule-id"
	NodeNameFlag                        = "node-name"
	OpSuccess                  opStatus = "SUCCEEDED"
	OpFailed                   opStatus = "FAILED"
	CrusoeAPIEndpointFlag               = "crusoe-api-endpoint"
	CrusoeAccessKeyFlag                 = "crusoe-elb-access-key"
	CrusoeSecretKeyFlag                 = "crusoe-elb-secret-key" //nolint:gosec // false positive, this is a flag name
	CrusoeProjectIDFlag                 = "crusoe-project-id"
	CrusoeVPCIDFlag                     = "crusoe-vpc-id"
	CrusoeSubnetIDFlag                  = "crusoe-subnet-id"
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
	// Load balancers created before UDP support never carry it, and keep the pre-UDP behaviour
	// exactly: upgrading the controller leaves their listeners and firewall rule as they were.
	AnnotationLoadBalancerProtocol = "crusoe.ai/load-balancer-protocol"
)

// firewallRuleInFlightHoldLimit bounds how long deleteFirewallRule waits for a rule's create
// operation to settle before resolving the rule by name instead, measured from the deletion
// request. A rule takes seconds to create; an operation still pending after this is not going
// to land, and there is no root cause an operator could fix to make it.
//
// API errors are deliberately not bounded the same way. Holding the finalizer on a failing
// delete is what makes cleanup self-heal: once the cause (a rotated key, an outage) is fixed,
// the retry succeeds and nothing is left behind. Releasing early would trade a Service stuck
// in Terminating for an orphaned ALLOW rule that nothing cleans up. The load balancer
// deletion in handleDelete already holds this way and shares the API key, so releasing the
// rule alone would leak it without unblocking anything. A Service that has to go before the
// cause is fixed is released by hand; the README's Firewall Rules section describes how.
const firewallRuleInFlightHoldLimit = 5 * time.Minute

// firewallRuleCreateRetryDelay is how long ensureFirewallRule waits, after a firewall rule's
// create operation fails, before submitting another. Most failures repeat (a quota, input the
// API rejects), so retrying at the in-progress poll rate would only pile up failed operations;
// this is short enough that a transient failure still clears within a couple of minutes.
const firewallRuleCreateRetryDelay = 2 * time.Minute

var (
	errUnableToGetOpRes        = errors.New("failed to get result of operation")
	errMixedPortProtocols      = errors.New("service mixes TCP and UDP ports; one protocol per load balancer")
	errUnsupportedPortProtocol = errors.New("unsupported service port protocol")

	// errFirewallRuleOperationUnresolvable marks an operation id that can never yield a rule
	// id: the record is gone or its result is unreadable. Retrying is pointless; recover
	// through the rule's deterministic name instead.
	errFirewallRuleOperationUnresolvable = errors.New("firewall rule operation is unresolvable")

	// errFirewallRuleOperationInFlight is returned by deleteFirewallRule while the rule's
	// create operation has not settled, so the caller holds the deletion instead of orphaning
	// the rule that is about to appear.
	errFirewallRuleOperationInFlight = errors.New("firewall rule operation still in flight")
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

// crusoeUserAgent identifies this controller in every request it makes to the Crusoe API.
const crusoeUserAgent = "crusoe-external-load-balancer-controller/0.0.1"

// GetLoadBalancerLookup returns the direct load balancer lookup handleCreate uses to adopt an
// existing load balancer, until client-go is upgraded (CRUSOE-111359). Call it after
// GetCrusoeClient, which binds the configuration it reads.
func GetLoadBalancerLookup() *crusoe.LoadBalancerLookup {
	return &crusoe.LoadBalancerLookup{
		BasePath:   viper.GetString(CrusoeAPIEndpointFlag),
		HTTPClient: crusoe.NewHTTPClient(viper.GetString(CrusoeAccessKeyFlag), viper.GetString(CrusoeSecretKeyFlag)),
		UserAgent:  crusoeUserAgent,
	}
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
		crusoeUserAgent,
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
// so logging it at Error would emit a permanent error line per Service per node event. It is
// also recorded as a ProtocolDrift Warning Event, because only the user can fix it.
func (r *ServiceReconciler) warnOnProtocolDrift(svc *corev1.Service, lbProtocol string, logger logr.Logger) {
	desired, err := DeriveLoadBalancerProtocol(svc)
	if err != nil {
		logger.Info("Service ports can no longer be served by its load balancer",
			"service", svc.Name, "namespace", svc.Namespace,
			"loadBalancerProtocol", lbProtocol, "reason", err.Error())

		r.recordWarning(svc, EventReasonProtocolDrift, fmt.Sprintf(
			"The %s load balancer cannot serve these ports: %s. Only the %s ports are being load balanced.",
			protocolName(lbProtocol), err.Error(), protocolName(lbProtocol)))

		return
	}

	if desired != lbProtocol {
		logger.Info("Load balancer protocol cannot be changed after creation, recreate the Service to change it",
			"service", svc.Name, "namespace", svc.Namespace,
			"loadBalancerProtocol", lbProtocol, "servicePortProtocol", desired)

		r.recordWarning(svc, EventReasonProtocolDrift, fmt.Sprintf(
			"The load balancer was created for %s and its protocol cannot be changed. Recreate the Service to use %s.",
			protocolName(lbProtocol), protocolName(desired)))
	}
}

func (r *ServiceReconciler) updateLoadBalancer(
	ctx context.Context,
	svc *corev1.Service,
	logger logr.Logger,
) error {

	loadBalancerID := svc.Annotations[loadbalancerIDLabelKey]

	// Load balancers created before UDP support carry no protocol annotation, and an annotation
	// holding an unrecognised value is ignored. Both fall back to offering every port as a
	// listener, exactly as this controller behaved before UDP support, so upgrading changes
	// nothing for them.
	ports := svc.Spec.Ports
	if lbProtocol := svc.Annotations[AnnotationLoadBalancerProtocol]; isKnownLoadBalancerProtocol(lbProtocol) {
		r.warnOnProtocolDrift(svc, lbProtocol, logger)

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
	_, httpResp, err := r.CrusoeClient.ExternalLoadBalancersApi.UpdateExternalLoadBalancer(ctx, lbUpdatePayload, projectId, loadBalancerID)
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
	// The update API reports back an operation rather than the load balancer itself, so its
	// load balancer value is empty. Log what was actually applied instead of an all-empty
	// struct.
	logger.Info("Successfully updated external load balancer",
		"lbID", loadBalancerID, "listeners", len(listenPortsAndBackends))

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

// findFirewallRuleByName looks up a VPC firewall rule by name and returns its id.
//
// Rule names come from GenerateLoadBalancerResourceName and embed a hash of the Service UID,
// so a match is the rule this controller created for the Service even when the operation
// that created it can no longer be retrieved.
func (r *ServiceReconciler) findFirewallRuleByName(ctx context.Context, projectID, ruleName string) (string, bool, error) {
	rules, _, err := r.CrusoeClient.VPCFirewallRulesApi.ListVPCFirewallRules(ctx, projectID)
	if err != nil {
		return "", false, err
	}

	for _, rule := range rules.Items {
		if rule.Name == ruleName {
			return rule.Id, true, nil
		}
	}

	return "", false, nil
}

// adoptFirewallRuleByName records an existing rule carrying the Service's rule name as this
// Service's firewall rule and drops any operation id that can no longer be relied on. It
// reports whether such a rule was found; when it was, the Service has been patched.
func (r *ServiceReconciler) adoptFirewallRuleByName(
	ctx context.Context, svc, svcCopy *corev1.Service, args *firewallRuleArgs,
) (bool, error) {
	logger := log.FromContext(ctx)

	ruleID, found, err := r.findFirewallRuleByName(ctx, args.projectID, args.ruleName)
	if err != nil {
		logger.Error(err, "Failed to list firewall rules")

		return false, err
	}

	if !found {
		return false, nil
	}

	logger.Info("Adopted existing firewall rule", "name", args.ruleName, "ruleID", ruleID)
	svc.Annotations[FirewallRuleIdKey] = ruleID
	delete(svc.Annotations, FirewallRuleOperationIdKey)

	return true, r.Patch(ctx, svc, client.MergeFrom(svcCopy))
}

// firewallRuleRetryWait returns how much of firewallRuleCreateRetryDelay is left after a
// failed firewall rule create operation, measured from when it completed (or started, if the
// completion time is missing). It returns zero once the delay has passed, and when the
// operation carries no usable timestamp, so a retry is never blocked indefinitely. The
// timestamp comes from the server's clock, so the wait is capped at the delay itself: a
// server clock running ahead of this one cannot stretch it.
func firewallRuleRetryWait(op *swagger.Operation, now time.Time) time.Duration {
	for _, timestamp := range []string{op.CompletedAt, op.StartedAt} {
		settled, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			continue
		}

		return min(firewallRuleCreateRetryDelay, max(0, settled.Add(firewallRuleCreateRetryDelay).Sub(now)))
	}

	return 0
}

// ensureFirewallRule reconciles the VPC firewall rule backing a Service.
//
// The Crusoe API creates a firewall rule through an asynchronous operation, so a single
// pass cannot always finish the job: the rule's id only becomes available once that
// operation completes. requeue reports whether the caller must reconcile again to finish
// recording the rule. Returning false with a nil error means there is nothing more to do on
// this pass: the rule is settled and its id stored in the FirewallRuleIdKey annotation, or
// the Service does not ask for a rule at all.
func (r *ServiceReconciler) ensureFirewallRule(ctx context.Context, svc *corev1.Service) (requeueAfter time.Duration, err error) {
	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}
	svcCopy := svc.DeepCopy()

	if val, exists := svc.Annotations[ManageFirewallRuleKey]; !exists || val != ManageFirewallRuleEnabled {
		return 0, nil
	}

	logger := log.FromContext(ctx)

	// Resolve the args before looking at a pending operation, but never mistake "could not find
	// out" for "nothing to do": an error here is returned so the pass is retried, which matters
	// most when an operation id is waiting to be resolved.
	args, err := r.GetFirewallRuleArgs(ctx, svc)
	if err != nil {
		return 0, err
	}

	if args == nil {
		// A deliberate skip; see GetFirewallRuleArgs.
		return 0, nil
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
				return 0, err
			}
		} else {
			matches := CompareFirewallRule(ctx, &rule, args)
			if matches {
				logger.Info("Firewall rule matches service, no action needed")
				return 0, nil
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
				return 0, err
			}

			return 0, nil
		}
	} else if operationID, exists := svc.Annotations[FirewallRuleOperationIdKey]; exists {
		logger.Info("Firewall rule operation exists, checking status")

		op, firewallRule, err := GetFirewallRuleOperationResult(ctx, r.CrusoeClient, operationID)
		if err == nil && op == nil {
			err = fmt.Errorf("%w: operation %s returned no record", errFirewallRuleOperationUnresolvable, operationID)
		}

		// Every case that does not return falls out of the switch into the create path below,
		// having first dropped the operation id it no longer trusts.
		switch {
		case errors.Is(err, errFirewallRuleOperationUnresolvable):
			// This operation id will never yield a rule id: the record has aged out of the
			// operations store, or its result cannot be read. The rule it created, if any,
			// still carries the Service's deterministic name, so adopt that. If no such rule
			// exists either, the operation left nothing behind.
			logger.Info("Firewall rule operation is unresolvable, looking up the rule by name",
				"operationID", operationID, "reason", err.Error())

			adopted, adoptErr := r.adoptFirewallRuleByName(ctx, svc, svcCopy, args)
			if adoptErr != nil {
				return 0, adoptErr
			}

			if adopted {
				return 0, nil
			}

			logger.Info("No firewall rule found for the operation, recreating", "operationID", operationID)
			delete(svc.Annotations, FirewallRuleOperationIdKey)

		case err != nil:
			// Transient failure reading the operation. Keep its id and let the manager retry
			// with backoff; dropping it would resubmit a create for a rule that is most likely
			// about to exist.
			logger.Error(err, "Failed to get firewall rule operation result", "operationID", operationID)

			return 0, err

		case op.State == string(OpFailed):
			marshalledResult, _ := json.Marshal(op.Result) // ignore error or handle it
			resultText := strings.ToLower(string(marshalledResult))
			if strings.Contains(resultText, "already exists") {
				logger.Info("Firewall rule already exists, adopting it")

				adopted, adoptErr := r.adoptFirewallRuleByName(ctx, svc, svcCopy, args)
				if adoptErr != nil {
					return 0, adoptErr
				}

				if adopted {
					return 0, nil
				}

				// The API reported a name clash but no rule by that name is visible now, so it
				// was removed in the meantime. Create it again, after the same cooldown.
				logger.Info("Firewall rule reported as existing but not found")
			}

			// Do not resubmit straight away. A create that failed once usually fails again the
			// same way (a quota, input the API rejects), and resubmitting on every pass turned
			// that into a new create every ~10 seconds, forever. The failed operation's id stays
			// on the Service until the cooldown has passed since it completed, so every
			// reconcile in between waits too, whatever triggered it.
			if wait := firewallRuleRetryWait(op, time.Now()); wait > 0 {
				logger.Info("Firewall rule creation failed, will retry later",
					"operationID", operationID, "retryIn", wait.String(), "result", op.Result)

				return wait, nil
			}

			logger.Info("Retrying failed firewall rule creation", "operationID", operationID, "result", op.Result)
			delete(svc.Annotations, FirewallRuleOperationIdKey)

		case op.State == string(OpSuccess):
			logger.Info("Firewall rule operation complete", "ruleInfo", firewallRule)
			svc.Annotations[FirewallRuleIdKey] = firewallRule.Id

			return 0, r.Patch(ctx, svc, client.MergeFrom(svcCopy))

		default:
			// The operation has not settled yet, so the rule's id is not available to record.
			// Nothing guarantees another reconcile of this Service, so ask for one rather than
			// reporting success and leaving FirewallRuleIdKey unwritten until unrelated churn
			// happens to trigger one.
			logger.Info("Firewall rule operation in progress, will retry")

			return firewallRuleRequeueInterval, nil
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
		return 0, err
	}

	svc.Annotations[FirewallRuleOperationIdKey] = op_resp.Operation.OperationId
	logger.Info("Created firewall rule operation", "name", args.ruleName, "operationId", op_resp.Operation.OperationId)

	// The operation was only just submitted. Requeue so a later pass can read its result
	// and store the rule id; without this the annotation is never written.
	return firewallRuleRequeueInterval, r.Patch(ctx, svc, client.MergeFrom(svcCopy))
}

// deleteFirewallRule removes the VPC firewall rule backing a Service that is being deleted.
//
// A non-nil error keeps the Service's finalizer so the deletion is retried with backoff and
// completes on its own once the cause clears; see firewallRuleInFlightHoldLimit for why that
// is not time-bounded. Errors wrapping errFirewallRuleOperationInFlight ask the caller to poll
// on a fixed interval rather than back off.
func (r *ServiceReconciler) deleteFirewallRule(ctx context.Context, svc *corev1.Service) error {
	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}

	if val, exists := svc.Annotations[ManageFirewallRuleKey]; !exists || val != ManageFirewallRuleEnabled {
		return nil
	}

	logger := log.FromContext(ctx)

	projectId := viper.GetString(CrusoeProjectIDFlag)
	if projectId == "" {
		// Nothing can have been created without a project id, and GetFirewallRuleArgs treats
		// the same condition as "skip". Blocking the finalizer here would strand the Service.
		logger.Info("CRUSOE_PROJECT_ID not set, skipping firewall rule deletion", "service", svc.Name)

		return nil
	}

	// Resolve the rule id: the recorded id if there is one, otherwise through the create
	// operation, otherwise through the rule's deterministic name.
	ruleID := svc.Annotations[FirewallRuleIdKey]

	if operationID := svc.Annotations[FirewallRuleOperationIdKey]; ruleID == "" && operationID != "" {
		// Wait for an in-flight create operation, but not forever: past the limit, resolve
		// through the rule's name instead.
		holdExpired := svc.DeletionTimestamp != nil &&
			time.Since(svc.DeletionTimestamp.Time) > firewallRuleInFlightHoldLimit

		op, firewallRule, err := GetFirewallRuleOperationResult(ctx, r.CrusoeClient, operationID)
		switch {
		case err == nil && firewallRule != nil:
			ruleID = firewallRule.Id

		case err == nil && op != nil && op.State != string(OpFailed) && !holdExpired:
			// The create operation has not settled, so the rule is about to exist. Removing
			// the finalizer now would orphan it; hold the deletion until the operation lands.
			return fmt.Errorf("%w: operation %s", errFirewallRuleOperationInFlight, operationID)

		case err != nil && !errors.Is(err, errFirewallRuleOperationUnresolvable):
			// Transient failure reading the operation. Retry rather than guess.
			return err

		default:
			// Failed, unresolvable, or still in flight past the hold limit: fall back to the
			// rule's name below.
			if err == nil && op != nil && op.State != string(OpFailed) {
				logger.Info("Firewall rule operation still pending past the hold limit, resolving by name",
					"service", svc.Name, "operationID", operationID)
			}
		}
	}

	if ruleID == "" {
		// No usable id from the annotations. That covers a failed or vanished operation, an
		// operation id that never got recorded, and annotations removed by hand. The rule, if
		// it exists, still carries the Service's deterministic name, so look for it that way
		// rather than leaving it behind.
		ruleName, nameErr := GenerateLoadBalancerResourceName(svc.Namespace, svc.Name, string(svc.UID))
		if nameErr != nil {
			// The name could not have been generated at create time either, so no rule was
			// ever made for this Service.
			logger.Error(nameErr, "Failed to derive firewall rule name, nothing to delete", "service", svc.Name)

			return nil
		}

		foundID, found, findErr := r.findFirewallRuleByName(ctx, projectId, ruleName)
		if findErr != nil {
			return findErr
		}

		if !found {
			logger.Info("No firewall rule found for Service, nothing to delete",
				"service", svc.Name, "name", ruleName)

			return nil
		}

		ruleID = foundID
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

// GetFirewallRuleArgs builds the firewall rule a Service should have.
//
// It distinguishes "nothing to do" from "could not find out". A nil result with a nil error is
// a deliberate skip that retrying cannot change: no project id, a name that cannot be generated,
// no port matching the load balancer's protocol, or no backends yet (a Node event brings the
// Service back once there are). An error means the answer is unknown, typically a transient API
// failure, and the caller must retry. Treating that as a skip used to leave a new Service without
// its firewall rule, and so without traffic, until some unrelated event came along.
func (r *ServiceReconciler) GetFirewallRuleArgs(ctx context.Context, svc *corev1.Service) (*firewallRuleArgs, error) {
	logger := log.FromContext(ctx)
	projectID := viper.GetString(CrusoeProjectIDFlag)
	if projectID == "" {
		logger.Info("CRUSOE_PROJECT_ID not set, skipping firewall rule creation")
		return nil, nil
	}

	vpcID, _, err := getVPCAndLocationInfo(ctx, r.CrusoeClient, logger)
	if err != nil {
		return nil, fmt.Errorf("failed to get VPC info for the firewall rule: %w", err)
	}

	ruleName, err := GenerateLoadBalancerResourceName(svc.Namespace, svc.Name, string(svc.UID))
	if err != nil {
		// Only a namespace too long to fit the name limit gets here, which no retry changes.
		logger.Error(err, "Failed to generate firewall rule name, skipping",
			"service", svc.Name, "namespace", svc.Namespace)
		return nil, nil
	}

	sources := []swagger.FirewallRuleObject{{Cidr: "0.0.0.0/0"}}
	if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
		sources = make([]swagger.FirewallRuleObject, len(svc.Spec.LoadBalancerSourceRanges))
		for i, source := range svc.Spec.LoadBalancerSourceRanges {
			sources[i] = swagger.FirewallRuleObject{Cidr: source}
		}
	}

	// The firewall rule must follow the load balancer's protocol, not whatever the Service's
	// ports say today. Editing a port's protocol cannot change the load balancer (its protocol
	// is fixed at creation, see updateLoadBalancer), and a rule that followed the edit would
	// cut off traffic and health checks to a load balancer that is still serving. Load balancers
	// from before UDP support carry no recorded protocol and keep the original, unfiltered rule,
	// so upgrading never rewrites their firewall rule.
	ports := svc.Spec.Ports
	if lbProtocol := svc.Annotations[AnnotationLoadBalancerProtocol]; isKnownLoadBalancerProtocol(lbProtocol) {
		ports = servicePortsForProtocol(svc.Spec.Ports, lbProtocol)
		if len(ports) == 0 && len(svc.Spec.Ports) > 0 {
			logger.Info("No Service ports match the load balancer protocol, leaving the firewall rule unchanged",
				"service", svc.Name, "namespace", svc.Namespace, "loadBalancerProtocol", lbProtocol)

			return nil, nil
		}
	}

	protocols := []string{}
	for _, port := range ports {
		protocol := strings.ToLower(string(effectivePortProtocol(port)))
		if !slices.Contains(protocols, protocol) {
			protocols = append(protocols, protocol)
		}
	}

	destinationPorts := []string{}
	listenPortsAndBackends := r.parseListenPortsAndBackends(ctx, ports, logger)
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
		return nil, nil
	}

	return &firewallRuleArgs{
		projectID:        projectID,
		vpcID:            vpcID,
		ruleName:         ruleName,
		destinationPorts: destinationPorts,
		protocols:        protocols,
		sources:          sources,
	}, nil
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

// GetFirewallRuleOperationResult fetches a firewall rule operation and, once it has
// succeeded, the rule it produced.
//
// An error wrapping errFirewallRuleOperationUnresolvable means this operation id will never
// yield a rule id and the caller should recover through the rule's name. Any other error is
// transient: retry with the operation id intact.
func GetFirewallRuleOperationResult(ctx context.Context, crusoeClient *crusoeapi.APIClient, operationID string) (
	*swagger.Operation, *swagger.VpcFirewallRule, error,
) {
	op, httpResp, err := crusoeClient.VPCFirewallRuleOperationsApi.GetNetworkingVPCFirewallRulesOperation(
		ctx, viper.GetString(utils.CrusoeProjectIDFlag), operationID,
	)
	if err != nil {
		// 404 and 410 mean the record is gone (aged out of the operations store); 400 means
		// the id itself is not one the API will ever accept. None of these can be retried
		// into a rule id.
		if httpResp != nil {
			switch httpResp.StatusCode {
			case http.StatusBadRequest, http.StatusNotFound, http.StatusGone:
				return nil, nil, fmt.Errorf("%w: operation %s: %s", errFirewallRuleOperationUnresolvable, operationID, httpResp.Status)
			}
		}

		return nil, nil, err
	}
	if httpResp.StatusCode != http.StatusOK {
		return &op, nil, fmt.Errorf("failed to get firewall rule operation: %s", httpResp.Status)
	}

	// Only a succeeded operation carries the rule in its result. A failed operation's result
	// is an error payload and an in-flight one has none; decoding either as a rule would fail
	// and hide the operation's actual state from the caller.
	if op.State != string(OpSuccess) {
		return &op, nil, nil
	}

	firewallRule, err := OpResultToItem[swagger.VpcFirewallRule](op.Result)
	if err != nil {
		return &op, nil, fmt.Errorf("%w: operation %s result: %v", errFirewallRuleOperationUnresolvable, operationID, err)
	}

	if firewallRule.Id == "" {
		return &op, nil, fmt.Errorf("%w: operation %s succeeded without a rule id", errFirewallRuleOperationUnresolvable, operationID)
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
