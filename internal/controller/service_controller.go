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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
			if err := r.handleDelete(ctx, svc); err != nil {
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
		// Future: Implement update logic here

	default:
		logger.Info("Unrecognized operation for Service", "operation", operation, "name", req.Name, "namespace", req.Namespace)
	}

	return ctrl.Result{}, nil
}

// handleCreate handles the creation of a LoadBalancer for the given Service
func (r *ServiceReconciler) handleCreate(ctx context.Context, svc *corev1.Service) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Define the NodePort Service name
	// https://stackoverflow.com/a/59917275, Service names are restricted to a maximum of 63 characters
	const maxNameLength = 63
	const suffix = "-nodeport"

	originalName := svc.Name

	if len(originalName)+len(suffix) > maxNameLength {
		originalName = originalName[:maxNameLength-len(suffix)]
	}

	nodePortServiceName := originalName + suffix

	// Check if the NodePort Service already exists
	existingNodePortService := &corev1.Service{}
	err := r.Client.Get(ctx, client.ObjectKey{
		Namespace: svc.Namespace,
		Name:      nodePortServiceName,
	}, existingNodePortService)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			// Return the error if it is not a "not found" error
			logger.Error(err, "Failed to check existence of NodePort Service", "nodePortService", nodePortServiceName)
			return ctrl.Result{}, err
		}
	} else {
		// Service already exists, log and skip creation
		logger.Info("NodePort Service already exists; skipping creation", "nodePortService", nodePortServiceName)
		return ctrl.Result{}, nil
	}

	//TODO: update status field in spec to reflect success/failure once we get SDN endpoint added

	// Service does not exist, proceed to create it
	// load balance can expose multiple ports for different protocols
	// node port should be aware of this spec
	var ports []corev1.ServicePort
	for _, port := range svc.Spec.Ports {
		newPort := corev1.ServicePort{
			Name:       port.Name,       // Keep the port name (e.g., "http").
			Protocol:   port.Protocol,   // Default is TCP, same as the LoadBalancer Service.
			Port:       port.Port,       // Same port number (e.g., 80).
			TargetPort: port.TargetPort, // Route to the same target port in Pods (e.g., 8080).
		}

		// Honor explicitly specified nodePort values
		if port.NodePort != 0 {
			newPort.NodePort = port.NodePort
		}

		ports = append(ports, newPort)
	}

	nodePortService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nodePortServiceName,
			Namespace: svc.Namespace,
			Labels: map[string]string{
				"crusoe.ai/crusoe-load-balancer-controller.generated":  "true",
				"crusoe.ai/crusoe-load-balancer-controller.parent-svc": svc.Name, // Parent service name
			},
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Ports:    ports,
			Selector: svc.Spec.Selector, // Ensure traffic is routed to the same pods
		},
	}

	// marks the NodePort service as a child resource of the LoadBalancer service
	// ensures that if LB service is deleted, the NodePort service is garbage-collected
	err = controllerutil.SetControllerReference(svc, nodePortService, r.Scheme)
	if err != nil {
		logger.Error(err, "Failed to set owner reference for NodePort Service", "nodePortService", nodePortServiceName)
		return ctrl.Result{}, err
	}

	// Create the NodePort Service in the cluster
	err = r.Client.Create(ctx, nodePortService)
	if err != nil {
		logger.Error(err, "Failed to create matching NodePort Service", "nodePortService", nodePortServiceName)
		return ctrl.Result{}, err
	}

	logger.Info("Successfully created matching NodePort Service", "nodePortService", nodePortServiceName)

	apiPayload := map[string]interface{}{
		"name":      svc.Name,
		"namespace": svc.Namespace,
		"ports":     svc.Spec.Ports,
	}
	payloadBytes, _ := json.Marshal(apiPayload)
	// temporarily using this dummy url to mock out load balancer api call
	// check it out: https://jsonplaceholder.typicode.com/
	reqURL := "https://jsonplaceholder.typicode.com/posts"

	// Make HTTP request to external API
	httpReq, _ := http.NewRequest("POST", reqURL, bytes.NewBuffer(payloadBytes))
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		logger.Error(err, "Failed to create load balancer via API")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		logger.Error(nil, "Unexpected response from API", "status", resp.StatusCode)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	logger.Info("Successfully created load balancer", "service", svc.Name)
	return ctrl.Result{}, nil
}

// handleDelete handles cleanup logic for a Service marked for deletion
func (r *ServiceReconciler) handleDelete(ctx context.Context, svc *corev1.Service) error {
	logger := log.FromContext(ctx)

	// Mock API URL for deletion
	reqURL := "https://jsonplaceholder.typicode.com/posts/" + svc.Name

	// Create delete request
	httpReq, err := http.NewRequest("DELETE", reqURL, nil)
	if err != nil {
		logger.Error(err, "Failed to create DELETE request", "service", svc.Name)
		return err
	}

	// TODO: We would probably add polling logic here
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		logger.Error(err, "Failed to delete resource via mock API", "service", svc.Name)
		return err
	}
	defer resp.Body.Close()

	// Check the response status
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		logger.Error(nil, "Unexpected response from mock API", "status", resp.StatusCode, "service", svc.Name)
		return fmt.Errorf("unexpected response from mock API: %d", resp.StatusCode)
	}

	logger.Info("Successfully deleted resource via mock API", "service", svc.Name)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Define a predicate to filter LoadBalancer services
	loadBalancerPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		svc, ok := obj.(*corev1.Service)
		return ok && svc.Spec.Type == corev1.ServiceTypeLoadBalancer
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		WithEventFilter(loadBalancerPredicate).
		Named("service").
		Complete(r)
}
