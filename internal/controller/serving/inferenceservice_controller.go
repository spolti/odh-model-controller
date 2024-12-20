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

package serving

import (
	"context"
	"errors"

	"github.com/go-logr/logr"
	kservev1alpha1 "github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	kservev1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	routev1 "github.com/openshift/api/route/v1"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	authv1 "k8s.io/api/rbac/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/opendatahub-io/odh-model-controller/internal/controller/constants"
	"github.com/opendatahub-io/odh-model-controller/internal/controller/serving/reconcilers"
	"github.com/opendatahub-io/odh-model-controller/internal/controller/utils"
)

// InferenceServiceReconciler reconciles a InferenceService object
type InferenceServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	clientReader client.Reader
	bearerToken  string

	MeshDisabled                   bool
	ModelRegistryEnabled           bool
	modelRegistrySkipTls           bool
	mmISVCReconciler               *reconcilers.ModelMeshInferenceServiceReconciler
	kserveServerlessISVCReconciler *reconcilers.KserveServerlessInferenceServiceReconciler
	kserveRawISVCReconciler        *reconcilers.KserveRawInferenceServiceReconciler
}

func NewInferenceServiceReconciler(setupLog logr.Logger, client client.Client, scheme *runtime.Scheme, clientReader client.Reader,
	kClient kubernetes.Interface, meshDisabled bool, modelRegistryReconcileEnabled, modelRegistrySkipTls bool, bearerToken string) *InferenceServiceReconciler {
	isvcReconciler := &InferenceServiceReconciler{
		Client:                         client,
		Scheme:                         scheme,
		clientReader:                   clientReader,
		MeshDisabled:                   meshDisabled,
		ModelRegistryEnabled:           modelRegistryReconcileEnabled,
		modelRegistrySkipTls:           modelRegistrySkipTls,
		mmISVCReconciler:               reconcilers.NewModelMeshInferenceServiceReconciler(client),
		kserveServerlessISVCReconciler: reconcilers.NewKServeServerlessInferenceServiceReconciler(client, clientReader, kClient),
		kserveRawISVCReconciler:        reconcilers.NewKServeRawInferenceServiceReconciler(client),
		bearerToken:                    bearerToken,
	}

	if modelRegistryReconcileEnabled {
		setupLog.Info("Model registry inference service reconciliation enabled.")
	} else {
		setupLog.Info("Model registry inference service reconciliation disabled. To enable model registry " +
			"reconciliation for InferenceService, please provide --model-registry-inference-reconcile flag.")
	}

	return isvcReconciler
}

// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferenceservices,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferenceservices/finalizers,verbs=get;list;watch;update

// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=virtualservices/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.istio.io,resources=gateways,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=security.istio.io,resources=peerauthentications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.istio.io,resources=authorizationpolicies,verbs=get;list
// +kubebuilder:rbac:groups=telemetry.istio.io,resources=telemetries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maistra.io,resources=servicemeshmembers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=maistra.io,resources=servicemeshmemberrolls,verbs=get;list;watch
// +kubebuilder:rbac:groups=maistra.io,resources=servicemeshcontrolplanes,verbs=get;list;watch;use
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes/custom-host,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings;rolebindings,verbs=get;list;watch;create;update;patch;watch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;podmonitors,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces;pods;endpoints,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;serviceaccounts;services,verbs=get;list;watch;create;update;delete;patch
// +kubebuilder:rbac:groups=authorino.kuadrant.io,resources=authconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=datasciencecluster.opendatahub.io,resources=datascienceclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dscinitialization.opendatahub.io,resources=dscinitializations,verbs=get;list;watch

// Reconcile performs the reconciling of the Openshift objects for a Kubeflow
// InferenceService.
func (r *InferenceServiceReconciler) ReconcileServing(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Initialize logger format
	logger := log.FromContext(ctx).WithValues("InferenceService", req.Name, "namespace", req.Namespace)
	// Get the InferenceService object when a reconciliation event is triggered (create,
	// update, delete)
	isvc := &kservev1beta1.InferenceService{}
	err := r.Client.Get(ctx, req.NamespacedName, isvc)
	if err != nil && apierrs.IsNotFound(err) {
		logger.Info("Stop InferenceService reconciliation")
		// InferenceService not found, so we check for any other inference services that might be using Kserve/ModelMesh
		// If none are found, we delete the common namespace-scoped resources that were created for Kserve/ModelMesh.
		err1 := r.DeleteResourcesIfNoIsvcExists(ctx, logger, req.Namespace)
		if err1 != nil {
			logger.Error(err1, "Unable to clean up resources")
			return ctrl.Result{}, err1
		}
		return ctrl.Result{}, nil
	} else if err != nil {
		logger.Error(err, "Unable to fetch the InferenceService")
		return ctrl.Result{}, err
	}

	if isvc.GetDeletionTimestamp() != nil {
		return reconcile.Result{}, r.onDeletion(ctx, logger, isvc)
	}

	// Check what deployment mode is used by the InferenceService. We have differing reconciliation logic for Kserve and ModelMesh
	IsvcDeploymentMode, err := utils.GetDeploymentModeForIsvc(ctx, r.Client, isvc)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch IsvcDeploymentMode {
	case constants.ModelMesh:
		logger.Info("Reconciling InferenceService for ModelMesh")
		err = r.mmISVCReconciler.Reconcile(ctx, logger, isvc)
	case constants.Serverless:
		logger.Info("Reconciling InferenceService for Kserve in mode Serverless")
		err = r.kserveServerlessISVCReconciler.Reconcile(ctx, logger, isvc)
	case constants.RawDeployment:
		logger.Info("Reconciling InferenceService for Kserve in mode RawDeployment")
		err = r.kserveRawISVCReconciler.Reconcile(ctx, logger, isvc)
	}

	return ctrl.Result{}, err
}

// Reconcile is the top-level function to run the different integrations with ODH platform:
// - Model Registry integration (opt-in via CLI flag)
// - OpenShift and other ODH features, like Data Connections, Routes and certificate trust.
func (r *InferenceServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	reconcileResult, reconcileErr := r.ReconcileServing(ctx, req)

	if r.ModelRegistryEnabled {
		mrReconciler := reconcilers.NewModelRegistryInferenceServiceReconciler(
			r.Client,
			log.FromContext(ctx).WithName("controllers").WithName("ModelRegistryInferenceService"),
			r.modelRegistrySkipTls,
			r.bearerToken,
		)

		mrResult, mrErr := mrReconciler.Reconcile(ctx, req)

		if mrResult.Requeue {
			reconcileResult.Requeue = true
		}

		if mrResult.RequeueAfter > 0 && (reconcileResult.RequeueAfter == 0 || mrResult.RequeueAfter < reconcileResult.RequeueAfter) {
			reconcileResult.RequeueAfter = mrResult.RequeueAfter
		}

		reconcileErr = errors.Join(reconcileErr, mrErr)
	}

	return reconcileResult, reconcileErr
}

// SetupWithManager sets up the controller with the Manager.
func (r *InferenceServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&kservev1beta1.InferenceService{}).
		Owns(&kservev1alpha1.ServingRuntime{}).
		Owns(&corev1.Namespace{}).
		Owns(&routev1.Route{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&authv1.ClusterRoleBinding{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&monitoringv1.ServiceMonitor{}).
		Owns(&monitoringv1.PodMonitor{}).
		Named("inferenceservice").
		Watches(&kservev1alpha1.ServingRuntime{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				logger := log.FromContext(ctx)
				logger.Info("Reconcile event triggered by serving runtime: " + o.GetName())
				inferenceServicesList := &kservev1beta1.InferenceServiceList{}
				opts := []client.ListOption{client.InNamespace(o.GetNamespace())}

				// Todo: Get only Inference Services that are deploying on the specific serving runtime
				err := r.Client.List(ctx, inferenceServicesList, opts...)
				if err != nil {
					logger.Info("Error getting list of inference services for namespace")
					return []reconcile.Request{}
				}

				if len(inferenceServicesList.Items) == 0 {
					logger.Info("No InferenceServices found for Serving Runtime: " + o.GetName())
					return []reconcile.Request{}
				}

				reconcileRequests := make([]reconcile.Request, 0, len(inferenceServicesList.Items))
				for _, inferenceService := range inferenceServicesList.Items {
					reconcileRequests = append(reconcileRequests, reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      inferenceService.Name,
							Namespace: inferenceService.Namespace,
						},
					})
				}
				return reconcileRequests
			})).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
				logger := log.FromContext(ctx)
				logger.Info("Reconcile event triggered by Secret: " + o.GetName())
				isvc := &kservev1beta1.InferenceService{}
				err := r.Client.Get(ctx, types.NamespacedName{Name: o.GetName(), Namespace: o.GetNamespace()}, isvc)
				if err != nil {
					if apierrs.IsNotFound(err) {
						return []reconcile.Request{}
					}
					logger.Error(err, "Error getting the inferenceService", "name", o.GetName())
					return []reconcile.Request{}
				}

				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{Name: o.GetName(), Namespace: o.GetNamespace()}},
				}
			}))

	return builder.Complete(r)
}

// general clean-up, mostly resources in different namespaces from kservev1beta1.InferenceService
func (r *InferenceServiceReconciler) onDeletion(ctx context.Context, log logr.Logger, inferenceService *kservev1beta1.InferenceService) error {
	log.V(1).Info("Running cleanup logic")

	IsvcDeploymentMode, err := utils.GetDeploymentModeForIsvc(ctx, r.Client, inferenceService)
	if err != nil {
		log.V(1).Error(err, "Could not determine deployment mode for ISVC. Some resources related to the inferenceservice might not be deleted.")
	}
	if IsvcDeploymentMode == constants.Serverless {
		log.V(1).Info("Deleting kserve inference resource (Serverless Mode)")
		return r.kserveServerlessISVCReconciler.OnDeletionOfKserveInferenceService(ctx, log, inferenceService)
	}
	if IsvcDeploymentMode == constants.RawDeployment {
		log.V(1).Info("Deleting kserve inference resource (RawDeployment Mode)")
		return r.kserveRawISVCReconciler.OnDeletionOfKserveInferenceService(ctx, log, inferenceService)
	}
	return nil
}

func (r *InferenceServiceReconciler) DeleteResourcesIfNoIsvcExists(ctx context.Context, log logr.Logger, namespace string) error {
	if err := r.kserveServerlessISVCReconciler.CleanupNamespaceIfNoKserveIsvcExists(ctx, log, namespace); err != nil {
		return err
	}
	if err := r.mmISVCReconciler.DeleteModelMeshResourcesIfNoMMIsvcExists(ctx, log, namespace); err != nil {
		return err
	}
	if err := r.kserveRawISVCReconciler.CleanupNamespaceIfNoKserveIsvcExists(ctx, log, namespace); err != nil {
		return err
	}
	return nil
}