/*

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

package webhook

import (
	"context"
	"fmt"
	kservev1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	"github.com/opendatahub-io/odh-model-controller/controllers/utils"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// +kubebuilder:webhook:admissionReviewVersions=v1beta1,path=/validate-isvc-v1beta1-service,mutating=false,failurePolicy=fail,groups="serving.kserve.io",resources=inferenceservices,verbs=create,versions=v1beta1,name=validating.isvc.odh-model-controller.opendatahub.io,sideEffects=None

type isvcValidator struct {
	client client.Client
}

func NewIsvcValidator(client client.Client) *isvcValidator {
	return &isvcValidator{client: client}
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type serving.kserve.io.inferenceServices
// This webhook will filter out the protected namespaces preventing the user to create the InferenceService in those namespaces
// which are: The Istio control plane namespace, Application namespace and Serving namespace
func (is *isvcValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (warnings admission.Warnings, err error) {
	protectedNamespaces := make([]string, 3)
	// hardcoding for now since there is no plan to install knative on other namespaces
	protectedNamespaces[0] = "knative-serving"

	log := logf.FromContext(ctx).WithName("InferenceServiceValidatingWebhook")

	isvc, ok := obj.(*kservev1beta1.InferenceService)
	if !ok {
		return nil, fmt.Errorf("expected a inferenceService but got a %T", obj)
	}

	log = log.WithValues("namespace", isvc.Namespace, "isvc", isvc.Name)
	log.Info("Validating InferenceService")

	_, meshNamespace := utils.GetIstioControlPlaneName(ctx, is.client)
	protectedNamespaces[1] = meshNamespace

	appNamespace, err := utils.GetApplicationNamespace(ctx, is.client)
	if err != nil {
		return nil, err
	}
	protectedNamespaces[2] = appNamespace

	log.Info("Filtering protected namespaces", "namespaces", protectedNamespaces)
	for _, ns := range protectedNamespaces {
		if isvc.Namespace == ns {
			log.V(1).Info("Namespace is protected, the InferenceService will not be created")
			return admission.Warnings{"Namespace is protected"}, fmt.Errorf("namespace %s is protected, "+
				"The InferenceService %s will not be created", ns, isvc.Name)
		}
	}
	log.Info("Namespace is not protected")
	return nil, nil

}

func (is *isvcValidator) ValidateUpdate(_ context.Context, _, _ runtime.Object) (warnings admission.Warnings, err error) {
	// Nothing to validate on updates
	return nil, nil
}

func (is *isvcValidator) ValidateDelete(_ context.Context, _ runtime.Object) (warnings admission.Warnings, err error) {
	// For deletion, we don't need to validate anything.
	return nil, nil
}
