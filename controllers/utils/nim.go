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

package utils

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/go-logr/logr"
	"github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	kserveconstants "github.com/kserve/kserve/pkg/constants"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"strings"
	"time"
)

type (
	// NimCatalogQuery is used for constructing a query for NIM catalog fetch
	NimCatalogQuery struct {
		Query    string `json:"query"`
		Page     int    `json:"page"`
		PageSize int    `json:"pageSize"`
	}

	// NimCatalogResponse represents the NIM catalog fetch response
	NimCatalogResponse struct {
		ResultPageTotal int `json:"resultPageTotal"`
		Params          struct {
			Page int `json:"page"`
		} `json:"params"`
		Results []struct {
			GroupValue string `json:"groupValue"`
			Resources  []struct {
				ResourceId string `json:"resourceId"`
				Attributes []struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				} `json:"attributes"`
			} `json:"resources"`
		} `json:"results"`
	}

	// NimTokenResponse represents the NIM token response
	NimTokenResponse struct {
		Token     string `json:"token"`
		ExpiresIn int    `json:"expires_in"`
	}

	// NimRuntime is a representation of a NIM custom runtime
	NimRuntime struct {
		Resource string
		Version  string
		Org      string
		Team     string
		Image    string
	}

	// NimModel is a representation of NIM model info
	NimModel struct {
		Name             string   `json:"name"`
		DisplayName      string   `json:"displayName"`
		ShortDescription string   `json:"shortDescription"`
		Namespace        string   `json:"namespace"`
		Tags             []string `json:"tags"`
		LatestTag        string   `json:"latestTag"`
		UpdatedDate      string   `json:"updatedDate"`
	}

	HttpClient interface {
		Do(*http.Request) (*http.Response, error)
	}
)

const (
	nimGetRuntimeTokenFmt    = "https://nvcr.io/proxy_auth?account=$oauthtoken&offline_token=true&scope=repository:%s:pull"
	nimGetRuntimeManifestFmt = "https://nvcr.io/v2/%s/manifests/%s"
	nimGetNgcCatalog         = "https://api.ngc.nvidia.com/v2/search/catalog/resources/CONTAINER"
	nimGetNgcToken           = "https://authn.nvidia.com/token?service=ngc&"
	nimGetNgcModelDataFmt    = "https://api.ngc.nvidia.com/v2/org/%s/team/%s/repos/%s?resolve-labels=true"
)

var NimHttpClient HttpClient

func init() {
	NimHttpClient = &http.Client{Timeout: time.Second * 30}
}

// GetAvailableNimRuntimes is used for fetching a list of available NIM custom runtimes
func GetAvailableNimRuntimes(logger logr.Logger) ([]NimRuntime, error) {
	return getNimRuntimes(logger, []NimRuntime{}, 0, 1000)
}

// ValidateApiKey is used for validating the given API key by retrieving the token and pulling the given custom runtime
func ValidateApiKey(logger logr.Logger, apiKey string, runtimes []NimRuntime) error {
	for _, runtime := range runtimes {
		tokenResp, tokenErr := getRuntimeRegistryToken(logger, apiKey, runtime.Resource)
		if tokenErr != nil {
			return tokenErr
		}

		manifestResp, manifestErr := attemptToPullManifest(logger, runtime, tokenResp)
		if manifestErr != nil {
			return manifestErr
		}

		switch manifestResp.StatusCode {
		case http.StatusUnavailableForLegalReasons:
			continue
		case http.StatusOK:
			return nil
		}
		break
	}
	return fmt.Errorf("failed to validate api key")
}

// GetNimModelData is used for fetching the model info for the given runtimes, returns configmap data
func GetNimModelData(logger logr.Logger, apiKey string, runtimes []NimRuntime) (map[string]string, error) {
	data := map[string]string{}
	tokenResp, tokenErr := getNgcToken(logger, apiKey)
	if tokenErr != nil {
		return data, tokenErr
	}

	for _, runtime := range runtimes {
		if model, unmarshaled := getModelData(logger, runtime, tokenResp); model != nil {
			data[model.Name] = unmarshaled
		}
	}

	if len(data) < 1 {
		return nil, fmt.Errorf("no models found")
	}
	return data, nil
}

// getNimRuntimes is used to send multiple requests to NVIDIA NIM runtimes endpoint, response pagination-based.
// it parses the runtimes from every response and returns a list of all runtimes
func getNimRuntimes(logger logr.Logger, runtimes []NimRuntime, page, pageSize int) ([]NimRuntime, error) {
	req, reqErr := http.NewRequest("GET", nimGetNgcCatalog, nil)
	if reqErr != nil {
		return runtimes, reqErr
	}

	params, _ := json.Marshal(NimCatalogQuery{Query: "orgName:nim", Page: page, PageSize: pageSize})
	query := req.URL.Query()
	query.Add("q", string(params))

	req.URL.RawQuery = query.Encode()

	resp, respErr := handleRequest(logger, req)
	if respErr != nil {
		return runtimes, respErr
	}

	body, bodyErr := io.ReadAll(resp.Body)
	if bodyErr != nil {
		return runtimes, bodyErr
	}

	catRes := &NimCatalogResponse{}
	if err := json.Unmarshal(body, catRes); err != nil {
		return runtimes, err
	}

	runtimes = append(runtimes, mapNimCatalogResponseToRuntimeList(catRes)...)
	if catRes.Params.Page < catRes.ResultPageTotal-1 {
		return getNimRuntimes(logger, runtimes, page+1, pageSize)
	}

	return runtimes, nil
}

// mapNimCatalogResponseToRuntimeList is used for parsing the ngc catalog response to a list of available runtimes
func mapNimCatalogResponseToRuntimeList(resp *NimCatalogResponse) []NimRuntime {
	var runtimes []NimRuntime
	for _, result := range resp.Results {
		if result.GroupValue == "CONTAINER" {
			for _, res := range result.Resources {
				for _, attribute := range res.Attributes {
					if attribute.Key == "latestTag" {
						parts := strings.Split(res.ResourceId, "/")
						runtimes = append(runtimes, NimRuntime{
							Resource: res.ResourceId,
							Version:  attribute.Value,
							Org:      parts[0],
							Team:     parts[1],
							Image:    parts[2],
						})
						break
					}
				}
			}
		}
	}

	return runtimes
}

// getRuntimeRegistryToken is used for fetching the token required for accessing NIM's runtimes
func getRuntimeRegistryToken(logger logr.Logger, apiKey, repo string) (*NimTokenResponse, error) {
	req, reqErr := http.NewRequest("GET", fmt.Sprintf(nimGetRuntimeTokenFmt, repo), nil)
	if reqErr != nil {
		return nil, reqErr
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("$oauthtoken:%s", apiKey)))
	req.Header.Add("Authorization", fmt.Sprintf("Basic %s", encoded))

	return requestToken(logger, req)
}

// getNgcToken is used for fetching the token required for accessing NIM's models
func getNgcToken(logger logr.Logger, apiKey string) (*NimTokenResponse, error) {
	req, reqErr := http.NewRequest("GET", nimGetNgcToken, nil)
	if reqErr != nil {
		return nil, reqErr
	}

	req.Header.Add("Authorization", fmt.Sprintf("ApiKey %s", apiKey))

	return requestToken(logger, req)
}

// requestToken is used for sending a token requests and parse the response
func requestToken(logger logr.Logger, req *http.Request) (*NimTokenResponse, error) {
	resp, respErr := handleRequest(logger, req)
	if respErr != nil {
		return nil, respErr
	}

	body, bodyErr := io.ReadAll(resp.Body)
	if bodyErr != nil {
		return nil, bodyErr
	}

	tokenResponse := &NimTokenResponse{}
	if err := json.Unmarshal(body, tokenResponse); err != nil {
		return nil, err
	}

	return tokenResponse, nil
}

// attemptToPullManifest is used for pulling a runtime for verifying access
func attemptToPullManifest(logger logr.Logger, runtime NimRuntime, tokenResp *NimTokenResponse) (*http.Response, error) {
	req, reqErr := http.NewRequest("GET", fmt.Sprintf(nimGetRuntimeManifestFmt, runtime.Resource, runtime.Version), nil)
	if reqErr != nil {
		return nil, reqErr
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", tokenResp.Token))
	req.Header.Add("Accept", "application/vnd.oci.image.index.v1+json")

	return handleRequest(logger, req)
}

// getModelData is used for fetching NIM model data for the given runtime
func getModelData(logger logr.Logger, runtime NimRuntime, tokenResp *NimTokenResponse) (*NimModel, string) {
	req, reqErr := http.NewRequest("GET", fmt.Sprintf(nimGetNgcModelDataFmt, runtime.Org, runtime.Team, runtime.Image), nil)
	if reqErr != nil {
		logger.Error(reqErr, "failed to construct request")
		return nil, ""
	}

	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", tokenResp.Token))

	resp, respErr := handleRequest(logger, req)
	if respErr != nil {
		return nil, ""
	}

	if resp.StatusCode != http.StatusOK {
		sErr := fmt.Errorf("got response %s", resp.Status)
		if resp.StatusCode == http.StatusUnavailableForLegalReasons {
			logger.Info("content not available for legal reasons")
		} else {
			logger.Error(sErr, "unexpected response status code")
		}
		return nil, ""
	}

	body, bodyErr := io.ReadAll(resp.Body)
	if bodyErr != nil {
		logger.Error(bodyErr, "failed to read response body")
		return nil, ""
	}

	model := &NimModel{}
	if err := json.Unmarshal(body, model); err != nil {
		logger.Error(err, "failed to deserialize body")
		return nil, ""
	}
	return model, string(body)
}

func handleRequest(logger logr.Logger, req *http.Request) (*http.Response, error) {
	logger.V(1).Info(fmt.Sprintf("sending api request %s", req.URL))

	resp, doErr := NimHttpClient.Do(req)
	if doErr != nil {
		logger.Error(doErr, "failed to send request")
		return nil, doErr
	}

	logger.V(1).Info(fmt.Sprintf("got api response %s", resp.Status))
	if resp.StatusCode != http.StatusOK {
		if resp.ContentLength > 0 {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				logger.V(1).Error(err, "failed to parse body")
			} else {
				logger.V(1).Info(fmt.Sprintf("got body %s", string(body)))
			}
		}
	}
	return resp, nil
}

// GetNimServingRuntimeTemplate returns the Template used by ODH for creating serving runtimes
func GetNimServingRuntimeTemplate(scheme *runtime.Scheme) (*v1alpha1.ServingRuntime, error) {
	multiModel := false
	sr := &v1alpha1.ServingRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				"opendatahub.io/recommended-accelerators": "[\"nvidia.com/gpu\"]",
				"openshift.io/display-name":               "NVIDIA NIM",
			},
			Labels: map[string]string{
				"opendatahub.io/dashboard": "true",
			},
			Name: "nvidia-nim-runtime",
		},
		Spec: v1alpha1.ServingRuntimeSpec{
			ServingRuntimePodSpec: v1alpha1.ServingRuntimePodSpec{
				Annotations: map[string]string{
					"prometheus.io/path":                    "/metrics",
					"prometheus.io/port":                    "8000",
					"serving.knative.dev/progress-deadline": "30m",
				},
				Containers: []corev1.Container{
					{Env: []corev1.EnvVar{
						{
							Name:  "NIM_CACHE_PATH",
							Value: "/mnt/models/cache",
						},
						{
							Name: "NGC_API_KEY",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									Key: "NGC_API_KEY",
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "nvidia-nim-secrets",
									},
								},
							},
						},
					},
						Image: "",
						Name:  "kserve-container",
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 8000,
								Protocol:      corev1.ProtocolTCP,
							},
						},
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
								"nvidia.com/gpu":      resource.MustParse("2"),
							},
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
								"nvidia.com/gpu":      resource.MustParse("2"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								MountPath: "/dev/shm",
								Name:      "shm",
							},
							{
								MountPath: "/mnt/models/cache",
								Name:      "nim-pvc",
							},
						},
					},
				},
				ImagePullSecrets: []corev1.LocalObjectReference{
					{
						Name: "ngc-secret",
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "nim-pvc",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: "nim-pvc",
							},
						},
					},
				},
			},
			MultiModel: &multiModel,
			ProtocolVersions: []kserveconstants.InferenceServiceProtocol{
				kserveconstants.ProtocolGRPCV2,
				kserveconstants.ProtocolV2,
			},
			SupportedModelFormats: []v1alpha1.SupportedModelFormat{{Name: "replace-me"}},
		},
	}

	gvk, err := apiutil.GVKForObject(sr, scheme)
	if err != nil {
		return nil, err
	}
	sr.SetGroupVersionKind(gvk)

	return sr, nil
}
