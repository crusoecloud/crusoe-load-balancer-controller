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
	"fmt"
	"net/http"
	"time"

	utils "lb_controller/internal/utils"

	crusoeapi "github.com/crusoecloud/client-go/swagger/v1alpha5"
	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	CrusoeClient *crusoeapi.APIClient
}

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=services/finalizers,verbs=update

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
			if _, err := r.handleDelete(ctx, svc); err != nil {
				logger.Error(err, "Failed to handle delete operation", "name", req.Name, "namespace", req.Namespace)
				return ctrl.Result{}, err
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

	// Add the finalizer if not present
	if !controllerutil.ContainsFinalizer(svc, finalizer) {
		logger.Info("Adding finalizer to Service", "name", req.Name, "namespace", req.Namespace)
		controllerutil.AddFinalizer(svc, finalizer)
		if err := r.Update(ctx, svc); err != nil {
			logger.Error(err, "Failed to add finalizer", "name", req.Name, "namespace", req.Namespace)
			return ctrl.Result{}, err
		}
	}

	// Determine CRUD operation
	// TODO: in the future we'd perform a GET against the LB API to check if there's a LB with the
	// name that we're looking for to know if it's a create or update operation
	var operation string
	if svc.CreationTimestamp.Time.Add(10 * time.Second).After(time.Now()) {
		// Treat recently created services as 'create'
		operation = "create"
	} else {
		operation = "update"
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

	listenPortsAndBackends := r.parseListenPortsAndBackends(ctx, svc, logger)

	healthCheckOptions := ParseHealthCheckOptionsFromAnnotations(svc.Annotations)

	// Prepare payload for the API call
	// get vpc id
	projectId := viper.GetString(CrusoeProjectIDFlag)
	cluster, _, err := r.CrusoeClient.KubernetesClustersApi.GetCluster(ctx, projectId, viper.GetString(CrusoeClusterIDFlag))
	if err != nil {
		logger.Error(err, "Failed to get cluster", "clusterID", viper.GetString(CrusoeClusterIDFlag))
		return ctrl.Result{}, err
	}
	subnet, _, err := r.CrusoeClient.VPCSubnetsApi.GetVPCSubnet(ctx, projectId, cluster.SubnetId)
	if err != nil {
		logger.Error(err, "Failed to get vpc network id from cluster subnet id ", "subnetID", cluster.SubnetId)
		return ctrl.Result{}, err
	}

	apiPayload := crusoeapi.ExternalLoadBalancerPostRequest{
		VpcId:                  subnet.VpcNetworkId,
		Name:                   svc.Name,
		Location:               subnet.Location,
		Protocol:               "LOAD_BALANCER_PROTOCOL_TCP", // only TCP supported currently
		ListenPortsAndBackends: listenPortsAndBackends,
		HealthCheckOptions:     healthCheckOptions,
	}

	op_resp, http_resp, err := r.CrusoeClient.ExternalLoadBalancersApi.CreateExternalLoadBalancer(ctx, apiPayload, projectId)
	if err != nil {
		logger.Error(err, "Failed to create load balancer via API")
		return ctrl.Result{}, nil
	}

	if http_resp.StatusCode != http.StatusOK && http_resp.StatusCode != http.StatusCreated {
		logger.Error(nil, "Unexpected response from API", "status", http_resp.StatusCode)
		return ctrl.Result{}, nil
	}

	op, err := utils.WaitForOperation(ctx, "Creating ELB ...",
		op_resp.Operation, projectId, r.CrusoeClient.ExternalLoadBalancerOperationsApi.GetExternalLoadBalancerOperation)
	if err != nil || op.State != string(OpSuccess) {
		logger.Error(err, "Failed to create LB Service", "name", svc.Name, "namespace", svc.Namespace)
		return ctrl.Result{}, err
	}

	loadBalancer, err := OpResultToItem[swagger.ExternalLoadBalancer](op.Result)
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("RESULT", "http_resp", op.Result)
	// update external IP of LB svc
	externalIP := loadBalancer.Vip
	patch := client.MergeFrom(svc.DeepCopy())
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
		{IP: externalIP},
	}
	if err := r.Client.Status().Patch(ctx, svc, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to patch service status with external IP: %w", err)
	}

	// Store the Load Balancer ID in the Service annotations
	if svc.Annotations == nil {
		svc.Annotations = make(map[string]string)
	}
	svc.Annotations[loadbalancerIDLabelKey] = loadBalancer.Id

	// Update the Service object in the Kubernetes API
	err = r.Client.Update(ctx, svc)
	if err != nil {
		logger.Error(err, "Failed to update Service with Load Balancer ID", "service", svc.Name)
		return ctrl.Result{}, err
	}

	logger.Info("Stored Load Balancer ID in Service annotations", "service", svc.Name, "loadBalancerID", loadBalancer.Id)
	return ctrl.Result{}, nil
}

// handleDelete handles cleanup logic for a Service marked for deletion
func (r *ServiceReconciler) handleDelete(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	loadBalancerID := svc.Annotations[loadbalancerIDLabelKey]

	// Call the API to delete the load balancer
	projectId := viper.GetString(CrusoeProjectIDFlag)
	_, httpResp, err := r.CrusoeClient.ExternalLoadBalancersApi.DeleteExternalLoadBalancer(ctx, projectId, loadBalancerID)
	if err != nil {
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
			// Some other status code => re-queue
			logger.Error(err, "Unexpected error code from LB deletion",
				"statusCode", statusCode, "service", svc.Name, "loadBalancerID", loadBalancerID)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}
	}

	// If err == nil, presumably a 2xx status => success
	if httpResp != nil && (httpResp.StatusCode == http.StatusOK || httpResp.StatusCode == http.StatusNoContent) {
		logger.Info("Successfully deleted LB", "service", svc.Name, "loadBalancerID", loadBalancerID)
		return ctrl.Result{}, nil
	}

	logger.Error(err, "Failed to delete load balancer via API (unparsable error)",
		"service", svc.Name, "loadBalancerID", loadBalancerID)

	return ctrl.Result{RequeueAfter: 10 * time.Second}, err

}

// handleUpdate handles updates to a Service of type LoadBalancer
func (r *ServiceReconciler) handleUpdate(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Update External LB if needed
	if err := r.updateLoadBalancer(ctx, svc, logger); err != nil {
		// Could choose to requeue on errors
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	logger.Info("Successfully handled update for ELB", "service", svc.Name)
	return ctrl.Result{}, nil
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
