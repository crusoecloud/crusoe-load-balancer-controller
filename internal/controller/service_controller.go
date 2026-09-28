/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"lb_controller/internal/crusoe"
	utils "lb_controller/internal/utils"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const CrusoeClusterIDFlag = "crusoe-cluster-id" //nolint:gosec // false positive, this is a flag name

// firewallRuleRequeueInterval is how long to wait before checking an in-flight firewall
// rule operation again.
const firewallRuleRequeueInterval = 10 * time.Second

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	CrusoeClient *crusoeapi.APIClient

	// APIReader reads straight from the API server, bypassing the informer cache. handleCreate
	// uses it to confirm no load balancer has been recorded on a Service before creating one,
	// because the cache can lag behind this controller's own writes. Optional: when nil, the
	// cached client is used instead.
	APIReader client.Reader

	// LoadBalancerLookup finds a load balancer this controller already created for a Service,
	// by its deterministic name, so handleCreate adopts it instead of creating a second one.
	// Optional: when nil, handleCreate always creates.
	LoadBalancerLookup *crusoe.LoadBalancerLookup

	// Recorder emits Events on Services, so problems only the user can fix show up in
	// `kubectl describe svc` for users who cannot read this controller's logs. Optional: when
	// nil, no Events are emitted. Without the events permission in the deployed ClusterRole the
	// API server rejects them; the recorder logs that and the controller carries on.
	Recorder record.EventRecorder
}

// Reasons for the Warning Events this controller emits on Services.
const (
	// EventReasonUnsupportedPortProtocol: no load balancer can be created for the Service's
	// ports (TCP and UDP mixed, or SCTP).
	EventReasonUnsupportedPortProtocol = "UnsupportedPortProtocol"
	// EventReasonProtocolDrift: the Service's ports no longer match the protocol its load
	// balancer was created with, which cannot be changed.
	EventReasonProtocolDrift = "ProtocolDrift"
	// EventReasonLoadBalancerCreateFailed: the load balancer's create operation failed, for
	// example a protocol the location does not offer. The controller retries with backoff.
	EventReasonLoadBalancerCreateFailed = "LoadBalancerCreateFailed"
)

// maxOperationResultText bounds how much of an operation's result goes into a log line or
// Event message; the result is an arbitrary payload from the API.
const maxOperationResultText = 512

// staleCacheRequeueInterval is how long to wait before reconciling again when the informer
// cache is found to be behind the API server, giving it time to catch up.
const staleCacheRequeueInterval = time.Second

// loadBalancerIngressRequeueInterval is how long to wait before trying again to publish a load
// balancer's VIP as the Service's external IP, when it could not be published yet.
const loadBalancerIngressRequeueInterval = 10 * time.Second

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Service object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.1/pkg/reconcile
func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Service object
	svc := &corev1.Service{}
	err := r.Client.Get(ctx, req.NamespacedName, svc)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			logger.Error(err, "Failed to fetch Service", "name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, err
		}
		// If the service is not found, it may have been deleted
		logger.Info("Service resource not found. Ignoring as it might be deleted", "name", req.Name, "namespace", req.Namespace)
		return ctrl.Result{}, nil
	}

	// Define the finalizer
	// This ensures that the Reconcile function will be called when the Service is deleted,
	// and the resource remains in a "terminating" state until the finalizer is removed. This
	// way the reconciler is able to "see" the service delete
	finalizer := "crusoe.ai/crusoe-load-balancer-controller.finalizer"

	// Check if the Service is marked for deletion
	if !svc.DeletionTimestamp.IsZero() {
		// Perform cleanup and remove the finalizer
		if controllerutil.ContainsFinalizer(svc, finalizer) {
			logger.Info("Handling Delete operation for Service", "name", req.Name, "namespace", req.Namespace)
			result, err := r.handleDelete(ctx, svc)
			if err != nil {
				logger.Error(err, "Failed to handle delete operation", "name", req.Name, "namespace", req.Namespace)
				return ctrl.Result{}, err
			}

			// Cleanup is not finished yet, for example a firewall rule operation is still in
			// flight. Keep the finalizer and come back when asked; removing it now would orphan
			// whatever is still being cleaned up.
			if !result.IsZero() {
				return result, nil
			}

			// Remove the finalizer
			logger.Info("Removing finalizer from Service", "name", req.Name, "namespace", req.Namespace)
			controllerutil.RemoveFinalizer(svc, finalizer)
			if err := r.Update(ctx, svc); err != nil {
				logger.Error(err, "Failed to remove finalizer", "name", req.Name, "namespace", req.Namespace)
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// A Service with a loadBalancerClass belongs to another controller, e.g. tailscale-operator.
	if svc.Spec.LoadBalancerClass != nil {
		return ctrl.Result{}, nil
	}

	// Add the finalizer if not present
	if !controllerutil.ContainsFinalizer(svc, finalizer) {
		logger.Info("Adding finalizer to Service", "name", req.Name, "namespace", req.Namespace)
		controllerutil.AddFinalizer(svc, finalizer)
		if err := r.Update(ctx, svc); err != nil {
			logger.Error(err, "Failed to add finalizer", "name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, err
		}
	}

	// Determine CRUD operation based on whether we've already created a load balancer for this service
	operation := "update"
	if _, exists := svc.Annotations[loadbalancerIDLabelKey]; !exists {
		operation = "create"
	}

	// Handle operations using a switch statement
	switch operation {
	case "create":
		logger.Info("Handling Create operation for Service", "name", req.Name, "namespace", req.Namespace)
		return r.handleCreate(ctx, svc)

	case "update":
		logger.Info("Handling Update operation for Service", "name", req.Name, "namespace", req.Namespace)
		return r.handleUpdate(ctx, svc)

	default:
		logger.Info("Unrecognized operation for Service", "operation", operation, "name", req.Name, "namespace", req.Namespace)
	}

	return ctrl.Result{}, nil
}

// handleCreate handles the creation of a LoadBalancer for the given Service
func (r *ServiceReconciler) handleCreate(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// A load balancer serves a single protocol, fixed at creation time, so it is derived
	// from the Service's ports before anything is created. A Service whose ports cannot be
	// served (mixed TCP and UDP, or SCTP) is a spec problem that a retry will not fix, so
	// the reconcile stops here without requeueing and the Service gets no external IP.
	//
	// Logged at Info rather than Error, like warnOnProtocolDrift: every Node event reconciles
	// every LoadBalancer Service again, so an Error here would repeat for each rejected Service
	// on every node change, for a condition only editing the Service can clear.
	protocol, err := DeriveLoadBalancerProtocol(svc)
	if err != nil {
		logger.Info("Cannot create load balancer for Service",
			"name", svc.Name, "namespace", svc.Namespace, "reason", err.Error())

		message := "Cannot create a load balancer: " + err.Error() + "."
		if errors.Is(err, errMixedPortProtocols) {
			message += " Split the Service into one Service per protocol."
		}

		r.recordWarning(svc, EventReasonUnsupportedPortProtocol, message)

		return ctrl.Result{}, nil
	}

	// The Service was read from the informer cache, which can be behind this controller's own
	// writes: a reconcile queued by an earlier write can start before the watch event carrying
	// the load balancer id arrives. Check the API server before creating anything, so a stale
	// cache cannot cause a second load balancer.
	stale, err := r.cachedServiceIsStale(ctx, svc)
	if err != nil {
		return ctrl.Result{}, err
	}

	if stale {
		logger.Info("Cached Service is behind the API server, waiting for the cache to catch up",
			"name", svc.Name, "namespace", svc.Namespace)

		return ctrl.Result{RequeueAfter: staleCacheRequeueInterval}, nil
	}

	listenPortsAndBackends := r.parseListenPortsAndBackends(ctx, servicePortsForProtocol(svc.Spec.Ports, protocol), logger)
	healthCheckOptions := ParseHealthCheckOptionsFromAnnotations(svc.Annotations)

	// Get VPC ID and location information
	vpcID, location, err := getVPCAndLocationInfo(ctx, r.CrusoeClient, logger)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Generate unique LB name using service UID for uniqueness across clusters
	// Service UID is globally unique and persists for the lifetime of the service
	lbName, err := GenerateLoadBalancerResourceName(svc.Namespace, svc.Name, string(svc.UID))
	if err != nil {
		logger.Error(err, "Failed to generate load balancer name", "service", svc.Name, "namespace", svc.Namespace)
		return ctrl.Result{}, err
	}
	logger.Info("Generated load balancer name", "lbName", lbName, "service", svc.Name, "namespace", svc.Namespace, "serviceUID", svc.UID)

	projectId := viper.GetString(CrusoeProjectIDFlag)

	// A load balancer with this Service's name may already exist: an earlier pass created it
	// but failed before recording its id on the Service, or lost track of the create operation.
	// The name is derived from the Service's UID, so such a load balancer belongs to this
	// Service. Adopt it rather than create a second one.
	existing, found, err := r.findExistingLoadBalancer(ctx, projectId, lbName)
	if err != nil {
		return ctrl.Result{}, err
	}

	if found {
		// Record the protocol the load balancer actually has. If the Service has since been
		// edited to ask for a different one, handleUpdate reports the drift.
		if isKnownLoadBalancerProtocol(existing.Protocol) {
			protocol = existing.Protocol
		}

		logger.Info("Adopting existing load balancer for Service",
			"service", svc.Name, "namespace", svc.Namespace, "loadBalancerID", existing.Id, "protocol", protocol)

		return r.recordLoadBalancer(ctx, svc, existing, protocol)
	}

	apiPayload := crusoeapi.ExternalLoadBalancerPostRequest{
		VpcId:                  vpcID,
		Name:                   lbName,
		Location:               location,
		Protocol:               protocol,
		ListenPortsAndBackends: listenPortsAndBackends,
		HealthCheckOptions:     healthCheckOptions,
	}
	logger.Info("Creating load balancer", "payload", apiPayload)

	op_resp, http_resp, err := r.CrusoeClient.ExternalLoadBalancersApi.CreateExternalLoadBalancer(ctx, apiPayload, projectId)
	if err != nil {
		logger.Error(err, "Failed to create load balancer via API")
		// Only retry if this is not a client error (4xx)
		if http_resp != nil && http_resp.StatusCode >= 400 && http_resp.StatusCode < 500 {
			logger.Info("Client error (4xx) detected, will not retry", "statusCode", http_resp.StatusCode)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to create load balancer: %w", err)
	}

	if http_resp.StatusCode != http.StatusOK && http_resp.StatusCode != http.StatusCreated {
		err := fmt.Errorf("unexpected status code from API: %d", http_resp.StatusCode)
		logger.Error(err, "Unexpected response from API")
		// Only retry if this is not a client error (4xx)
		if http_resp.StatusCode >= 400 && http_resp.StatusCode < 500 {
			logger.Info("Client error (4xx) detected, will not retry", "statusCode", http_resp.StatusCode)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	op, err := utils.WaitForOperation(ctx, "Creating ELB ...",
		op_resp.Operation, projectId, r.CrusoeClient.ExternalLoadBalancerOperationsApi.GetExternalLoadBalancerOperation)
	if err != nil {
		logger.Error(err, "Failed to create LB Service", "name", svc.Name, "namespace", svc.Namespace)
		return ctrl.Result{}, err
	}

	if op.State != string(OpSuccess) {
		// WaitForOperation reports a finished operation without an error even when it failed.
		// Returning that nil error meant no retry, and nothing but a log line to explain why the
		// Service stayed <pending>. Say so on the Service and retry with backoff; a retry that
		// finds the load balancer was created after all adopts it by name.
		details := operationResultText(op)
		err := fmt.Errorf("load balancer create operation %s finished %s: %s", op.OperationId, op.State, details)
		logger.Error(err, "Failed to create LB Service", "name", svc.Name, "namespace", svc.Namespace)

		r.recordWarning(svc, EventReasonLoadBalancerCreateFailed,
			"Creating the load balancer failed: "+details+". Retrying with backoff.")

		return ctrl.Result{}, err
	}

	loadBalancer, err := OpResultToItem[swagger.ExternalLoadBalancer](op.Result)
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("RESULT", "http_resp", op.Result)

	return r.recordLoadBalancer(ctx, svc, loadBalancer, protocol)
}

// recordLoadBalancer records a created or adopted load balancer on the Service, publishes its
// VIP as the Service's external IP, and brings up the Service's firewall rule.
func (r *ServiceReconciler) recordLoadBalancer(
	ctx context.Context, svc *corev1.Service, loadBalancer *swagger.ExternalLoadBalancer, protocol string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Record the load balancer on the Service before anything else. The id annotation is what
	// sends every later reconcile to handleUpdate instead of back to handleCreate, so until it
	// is persisted, a retry has to find the load balancer by name to avoid creating a second
	// one. It is a merge patch rather than an Update on purpose: a patch carries no
	// resourceVersion, so it cannot fail with a conflict when something else touched the
	// Service since it was read.
	annotationPatch := client.MergeFrom(svc.DeepCopy())
	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}
	svc.Annotations[loadbalancerIDLabelKey] = loadBalancer.Id
	// Record the protocol so later reconciles can tell which ports belong on this load
	// balancer, and can detect a Service edited to a protocol the load balancer cannot adopt.
	svc.Annotations[AnnotationLoadBalancerProtocol] = protocol

	if err := r.Patch(ctx, svc, annotationPatch); err != nil {
		logger.Error(err, "Failed to record load balancer id on Service", "service", svc.Name, "loadBalancerID", loadBalancer.Id)
		return ctrl.Result{}, err
	}

	logger.Info("Stored Load Balancer ID in Service annotations",
		"service", svc.Name, "loadBalancerID", loadBalancer.Id, "protocol", protocol)

	if loadBalancer.Vip == "" {
		// An adopted load balancer can still be provisioning. Its id is recorded, so
		// handleUpdate takes over and publishes the VIP once one is assigned.
		logger.Info("Load balancer has no VIP yet, will publish it once assigned",
			"service", svc.Name, "loadBalancerID", loadBalancer.Id)

		return ctrl.Result{RequeueAfter: loadBalancerIngressRequeueInterval}, nil
	}

	// Then publish the VIP. If this fails, the id is already recorded, so the retry ends up in
	// handleUpdate, which restores a missing external IP.
	if err := r.setLoadBalancerIngress(ctx, svc, loadBalancer.Vip); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure firewall rule has been created if necessary. Creation runs as an asynchronous
	// operation, so keep reconciling until the rule has settled and its id is recorded.
	requeueAfter, err := r.ensureFirewallRule(ctx, svc)
	if err != nil {
		// Retry on a fixed delay rather than returning the error. An error retries within
		// milliseconds, usually before the informer cache has seen the load balancer id written
		// above; the fresh read in handleCreate would still stop a second create, but there is
		// no reason to provoke it. From the next pass on, handleUpdate owns the firewall rule
		// and backs off normally.
		logger.Error(err, "Failed to ensure firewall rule, will retry")

		return ctrl.Result{RequeueAfter: firewallRuleRequeueInterval}, nil
	}

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// handleDelete handles cleanup logic for a Service marked for deletion
func (r *ServiceReconciler) handleDelete(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	loadBalancerID := svc.Annotations[loadbalancerIDLabelKey]

	// Cleaning up is what the finalizer is for. If the rule cannot be removed yet, keep the
	// finalizer and come back, rather than release the Service and orphan an ALLOW rule. This
	// holds on a failing API exactly as the load balancer deletion below does, so that both
	// complete on their own once the cause clears; only the wait for an in-flight create
	// operation is time-bounded. The load balancer deletion waits behind this step.
	if err := r.deleteFirewallRule(ctx, svc); err != nil {
		if errors.Is(err, errFirewallRuleOperationInFlight) {
			logger.Info("Firewall rule creation still in flight, holding deletion",
				"service", svc.Name, "namespace", svc.Namespace, "reason", err.Error())

			return ctrl.Result{RequeueAfter: firewallRuleRequeueInterval}, nil
		}

		logger.Error(err, "Failed to delete firewall rule, will retry")

		return ctrl.Result{}, err
	}

	// A Service can be deleted without ever having had a load balancer created for it,
	// for example when its ports were rejected. There is nothing to delete, and calling
	// the API with an empty id would keep the finalizer in place and leave the Service
	// stuck terminating.
	if loadBalancerID == "" {
		logger.Info("No load balancer recorded for Service, nothing to delete",
			"service", svc.Name, "namespace", svc.Namespace)

		return ctrl.Result{}, nil
	}

	// Call the API to delete the load balancer
	projectId := viper.GetString(CrusoeProjectIDFlag)
	_, httpResp, err := r.CrusoeClient.ExternalLoadBalancersApi.DeleteExternalLoadBalancer(ctx, projectId, loadBalancerID)
	if err != nil {
		if httpResp == nil {
			// Transport-level failure: there is no status code to branch on.
			logger.Error(err, "Failed to delete load balancer via API",
				"service", svc.Name, "loadBalancerID", loadBalancerID)

			return ctrl.Result{}, err
		}

		statusCode := httpResp.StatusCode
		switch statusCode {
		case http.StatusNotFound:
			// 404 => LB doesn't exist. Let service deletion proceed.
			logger.Info("Load balancer not found (404). Assuming already deleted",
				"service", svc.Name, "loadBalancerID", loadBalancerID)
			return ctrl.Result{}, nil

		case http.StatusOK, http.StatusNoContent:
			// Some clients might set err even for 2xx, but that’s unusual.
			// Possibly handle success or do nothing here.
			logger.Info("Successfully deleted LB (2xx).",
				"service", svc.Name, "loadBalancerID", loadBalancerID)
			return ctrl.Result{}, nil

		default:
			// Some other status code: retry with backoff.
			logger.Error(err, "Unexpected error code from LB deletion",
				"statusCode", statusCode, "service", svc.Name, "loadBalancerID", loadBalancerID)
			return ctrl.Result{}, err
		}
	}

	// If err == nil, presumably a 2xx status => success
	if httpResp != nil && (httpResp.StatusCode == http.StatusOK || httpResp.StatusCode == http.StatusNoContent) {
		logger.Info("Successfully deleted LB", "service", svc.Name, "loadBalancerID", loadBalancerID)
		return ctrl.Result{}, nil
	}

	// No error but not a status this code recognises as success. Retry with backoff; the
	// next attempt either sees the same odd response or a 404, and the 404 lets deletion
	// proceed. This used to return a nil error with a RequeueAfter, which Reconcile silently
	// dropped; now that Reconcile honours RequeueAfter, say what is meant.
	status := "no response"
	if httpResp != nil {
		status = httpResp.Status
	}
	err = fmt.Errorf("unexpected response deleting load balancer %s: %s", loadBalancerID, status)
	logger.Error(err, "Failed to delete load balancer via API", "service", svc.Name)

	return ctrl.Result{}, err
}

// handleUpdate handles updates to a Service of type LoadBalancer
func (r *ServiceReconciler) handleUpdate(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// handleCreate publishes the VIP only after recording the load balancer id, so a failure
	// between the two leaves a Service with a load balancer but no external IP. Restore it, but
	// without holding up the rest of the update: an adopted load balancer can still be waiting
	// for its VIP, and its listeners and firewall rule should not wait with it.
	ingressRequeue := r.ensureLoadBalancerIngress(ctx, svc)

	// Update External LB if needed
	if err := r.updateLoadBalancer(ctx, svc, logger); err != nil {
		// Could choose to requeue on errors
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Ensure firewall rule has been created if necessary. As in handleCreate, an in-flight
	// operation, or a failed one waiting out its retry delay, needs another pass later.
	requeueAfter, err := r.ensureFirewallRule(ctx, svc)
	if err != nil {
		// The load balancer id is long since in the cache here, so unlike handleCreate there is
		// no reason to avoid a fast retry: let the manager back off.
		logger.Error(err, "Failed to ensure firewall rule")

		return ctrl.Result{}, err
	}

	if next := earliestRequeue(requeueAfter, ingressRequeue); next > 0 {
		return ctrl.Result{RequeueAfter: next}, nil
	}

	logger.Info("Successfully handled update for ELB", "service", svc.Name)

	return ctrl.Result{}, nil
}

// cachedServiceIsStale re-reads svc from the API server and reports whether the cached copy
// handleCreate was given is out of date in a way that forbids creating a load balancer: the
// Service already records one, is being deleted, or no longer exists.
func (r *ServiceReconciler) cachedServiceIsStale(ctx context.Context, svc *corev1.Service) (bool, error) {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}

	fresh := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(svc), fresh); err != nil {
		switch {
		case apierrors.IsNotFound(err):
			// Deleted since it was cached; nothing to create.
			return true, nil

		case apierrors.IsForbidden(err):
			// Reading a single Service needs the "get" verb, which the cached client never
			// used. If the deployed ClusterRole lacks it, keep creating load balancers exactly
			// as before rather than stop creating them at all.
			log.FromContext(ctx).Info("Cannot read the Service from the API server, relying on the cache",
				"service", svc.Name, "namespace", svc.Namespace, "reason", err.Error())

			return false, nil

		default:
			return false, err
		}
	}

	_, recorded := fresh.Annotations[loadbalancerIDLabelKey]

	return recorded || !fresh.DeletionTimestamp.IsZero(), nil
}

// findExistingLoadBalancer looks for a load balancer this controller already created under
// name. Several matches mean duplicates were created before creation was made idempotent; the
// first is adopted and the others are reported, because nothing else will clean them up.
func (r *ServiceReconciler) findExistingLoadBalancer(
	ctx context.Context, projectID, name string,
) (*swagger.ExternalLoadBalancer, bool, error) {
	if r.LoadBalancerLookup == nil {
		return nil, false, nil
	}

	matches, err := r.LoadBalancerLookup.ByName(ctx, projectID, name)
	if err != nil {
		return nil, false, fmt.Errorf("failed to look up existing load balancer %s: %w", name, err)
	}

	if len(matches) == 0 {
		return nil, false, nil
	}

	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, lb := range matches {
			ids = append(ids, lb.Id)
		}

		log.FromContext(ctx).Error(nil,
			"Found several load balancers with this Service's name; adopting the first, the others are orphaned and should be deleted",
			"name", name, "loadBalancerIDs", ids)
	}

	return &matches[0], true, nil
}

// setLoadBalancerIngress publishes a load balancer's VIP as the Service's external IP.
func (r *ServiceReconciler) setLoadBalancerIngress(ctx context.Context, svc *corev1.Service, vip string) error {
	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: vip}}

	if err := r.Client.Status().Patch(ctx, svc, patch); err != nil {
		return fmt.Errorf("failed to patch service status with external IP: %w", err)
	}

	return nil
}

// ensureLoadBalancerIngress restores a Service's external IP from its load balancer when the
// Service has none. It makes no API call when the external IP is already set.
//
// It never fails the reconcile. When the external IP cannot be restored yet (the load balancer
// has no VIP assigned, or cannot be read) it returns how long to wait before trying again, and
// the rest of the update carries on in the meantime.
func (r *ServiceReconciler) ensureLoadBalancerIngress(ctx context.Context, svc *corev1.Service) time.Duration {
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		return 0
	}

	logger := log.FromContext(ctx)
	loadBalancerID := svc.Annotations[loadbalancerIDLabelKey]
	projectID := viper.GetString(CrusoeProjectIDFlag)

	loadBalancer, _, err := r.CrusoeClient.ExternalLoadBalancersApi.GetExternalLoadBalancer(ctx, projectID, loadBalancerID)
	if err != nil {
		logger.Error(err, "Failed to read the load balancer to restore the Service's external IP, will retry",
			"service", svc.Name, "loadBalancerID", loadBalancerID)

		return loadBalancerIngressRequeueInterval
	}

	if loadBalancer.Vip == "" {
		logger.Info("Load balancer has no VIP yet, will publish it once assigned",
			"service", svc.Name, "loadBalancerID", loadBalancerID)

		return loadBalancerIngressRequeueInterval
	}

	logger.Info("Restoring missing external IP from the load balancer",
		"service", svc.Name, "loadBalancerID", loadBalancerID, "vip", loadBalancer.Vip)

	if err := r.setLoadBalancerIngress(ctx, svc, loadBalancer.Vip); err != nil {
		logger.Error(err, "Failed to restore the Service's external IP, will retry", "service", svc.Name)

		return loadBalancerIngressRequeueInterval
	}

	return 0
}

// earliestRequeue returns the soonest of the requested requeue delays, ignoring zeros, which
// mean no requeue is needed.
func earliestRequeue(delays ...time.Duration) time.Duration {
	var earliest time.Duration

	for _, delay := range delays {
		if delay > 0 && (earliest == 0 || delay < earliest) {
			earliest = delay
		}
	}

	return earliest
}

// recordWarning emits a Warning Event on the Service. Repeats are cheap: the recorder merges
// identical Events and rate-limits them per object, so emitting on every reconcile does not
// flood the API server.
func (r *ServiceReconciler) recordWarning(svc *corev1.Service, reason, message string) {
	if r.Recorder == nil {
		return
	}

	r.Recorder.Event(svc, corev1.EventTypeWarning, reason, message)
}

// operationResultText renders an operation's result, typically the error a failed operation
// reports, for a log line or Event message.
func operationResultText(op *swagger.Operation) string {
	if op.Result == nil {
		return "no details reported"
	}

	text := fmt.Sprintf("%v", *op.Result)
	if encoded, err := json.Marshal(*op.Result); err == nil {
		text = string(encoded)
	}

	if len(text) > maxOperationResultText {
		text = text[:maxOperationResultText] + "..."
	}

	return text
}

// protocolName turns a load balancer protocol into the name a user knows it by: TCP or UDP.
func protocolName(lbProtocol string) string {
	return strings.TrimPrefix(lbProtocol, "LOAD_BALANCER_PROTOCOL_")
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Define a predicate to filter LoadBalancer services
	loadBalancerPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		return ok && svc.Spec.Type == corev1.ServiceTypeLoadBalancer
	})

	nodeCreateDeletePredicate := predicate.Funcs{
		// Called for new Node objects
		CreateFunc: func(e event.CreateEvent) bool {
			return true
		},
		// Called for Node updates e.g. node from not ready to ready
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, oldOk := e.ObjectOld.(*corev1.Node)
			newNode, newOk := e.ObjectNew.(*corev1.Node)
			if !oldOk || !newOk {
				return false
			}

			if isNodeIncluded(oldNode) != isNodeIncluded(newNode) {
				log.Log.Info("Node load balancer exclusion changed", "node", newNode.Name, "included", isNodeIncluded(newNode))
				return true
			}

			oldReady := isNodeReady(oldNode)
			newReady := isNodeReady(newNode)

			// Only trigger if the node transitioned from NotReady -> Ready or vice versa
			if oldReady != newReady {
				log.Log.Info("Node readiness changed", "node", newNode.Name, "oldReady", oldReady, "newReady", newReady)
				return true
			}

			return false // Skip other updates
		},
		// Called for Node deletions
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
		// Rare, generic events are usually no-ops here
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}

	mapNodeToLBServices := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {

		node, ok := obj.(*corev1.Node)
		if !ok {
			return nil
		}
		log.Log.Info("Node event received", "node", node.Name)
		// List all LB Services and enqueue each
		svcList := &corev1.ServiceList{}
		if err := r.Client.List(ctx, svcList); err != nil {
			// On error, return no requests
			return nil
		}

		var reqs []reconcile.Request
		for _, svc := range svcList.Items {
			if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Namespace: svc.Namespace,
						Name:      svc.Name,
					},
				})
			}
		}
		return reqs
	},
	)
	return ctrl.NewControllerManagedBy(mgr).
		// Watch Service objects of type=LoadBalancer
		For(
			&corev1.Service{},
			builder.WithPredicates(loadBalancerPredicate),
		).
		// Also watch Node events to update backends list
		Watches(
			&corev1.Node{},
			mapNodeToLBServices,
			builder.WithPredicates(nodeCreateDeletePredicate),
		).
		Complete(r)

}
