/** Copyright 2020-2023 Alibaba Group Holding Limited.

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

// Package scheduling contains the logic for the scheduling
package scheduling

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// nolint: lll
// +kubebuilder:webhook:admissionReviewVersions=v1,sideEffects=None,path=/mutate-v1-pod-scheduling,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create;update,versions=v1,name=mpod.scheduling.kb.io

// Injector injects scheduling info into pods.
type Injector struct {
	Client  client.Client
	decoder *admission.Decoder
}

const (
	// NeedSchedulingLabel is the label for scheduling
	NeedSchedulingLabel = "scheduling.v6d.io/enabled"
)

// Handle handles admission requests.
func (r *Injector) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithName("injector").WithName("Assembly")

	pod := &corev1.Pod{}
	if err := r.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	anno := pod.GetAnnotations()
	selector := anno["workloadSelector"]
	order := anno["scheduledOrder"]

	jobToNodes := make(map[string]int)

	for _, o := range strings.Split(order, ",") {
		n := strings.Split(o, "=")
		i, _ := strconv.Atoi(n[1])
		jobToNodes[n[0]] = i
	}

	kv := strings.Split(selector, "=")

	podList := &corev1.PodList{}
	if err := r.Client.List(ctx, podList, client.MatchingLabels{kv[0]: kv[1]}); err != nil {
		fmt.Println("faled to list pod", err)
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		node := pod.Spec.NodeName
		if node != "" {
			jobToNodes[node]--
		}
	}

	for i := range jobToNodes {
		if jobToNodes[i] > 0 {
			pod.Spec.NodeName = i
			break
		}
	}

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	logger.Info("Injecting the nodeselector!")
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// InjectDecoder injects the decoder.
func (r *Injector) InjectDecoder(d *admission.Decoder) error {
	r.decoder = d
	return nil
}
