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

// Package sidecar contains the logic for injecting vineyard sidecar
package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"

	"github.com/apache/skywalking-swck/operator/pkg/kubernetes"
	"github.com/v6d-io/v6d/k8s/apis/k8s/v1alpha1"
	"github.com/v6d-io/v6d/k8s/pkg/config/annotations"
	"github.com/v6d-io/v6d/k8s/pkg/config/labels"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// nolint: lll
// +kubebuilder:webhook:admissionReviewVersions=v1,sideEffects=None,path=/mutate-v1-pod-sidecar,mutating=true,failurePolicy=fail,groups="",resources=pods,verbs=create;update,versions=v1,name=mpod.sidecar.kb.io

// Injector injects vineyard sidecar container into Pods
type Injector struct {
	Client   client.Client
	decoder  *admission.Decoder
	Template kubernetes.Repo
}

// Handle handles admission requests.
func (r *Injector) Handle(ctx context.Context, req admission.Request) admission.Response {
	logger := log.FromContext(ctx).WithName("Injector")

	templatePod := &corev1.Pod{}
	pod := &corev1.Pod{}
	if err := r.decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	anno := pod.Annotations
	if v, ok := anno[annotations.SidecarNameAnno]; ok && v == "default" {
		// create the default sidecar cr
		sidecar := &v1alpha1.Sidecar{}
		// get the pod's label as cr's name
		l := pod.Labels
		keys := []string{}
		for k := range l {
			if !strings.Contains(k, labels.SidecarEnableLabel) {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return admission.Errored(http.StatusInternalServerError, fmt.Errorf("the pod doesn't contain a pod selector"))
		}

		sort.Strings(keys)
		selectorname := strings.Join([]string{keys[0], l[keys[0]]}, "-")
		sidecar.Name = selectorname + "-default-sidecar"
		sidecar.Namespace = pod.Namespace

		err := r.Client.Get(ctx, types.NamespacedName{Name: sidecar.Name, Namespace: sidecar.Namespace}, sidecar)
		if err != nil && !apierrors.IsNotFound(err) {
			logger.Info("Get sidecar cr failed", "error", err)
			return admission.Errored(http.StatusInternalServerError, err)
		}
		// if the default sidecar cr doesn't exist, create it
		if apierrors.IsNotFound(err) {
			sidecar.Spec.Replicas = 1
			// use default configurations
			sidecar.Spec.Selector = keys[0] + "=" + l[keys[0]]

			if err := r.Client.Create(ctx, sidecar); err != nil {
				logger.Error(err, "failed to create default sidecar cr")
				return admission.Errored(http.StatusInternalServerError, err)
			}
		} else {
			// the default sidecar cr exists, update it
			sidecar.Spec.Replicas++
			if err := r.Client.Update(ctx, sidecar); err != nil {
				logger.Error(err, "failed to update default sidecar cr")
				return admission.Errored(http.StatusInternalServerError, err)
			}
		}

		buf, err := r.Template.ReadFile("sidecar/injection-template.yaml")
		if err != nil {
			logger.Error(err, "failed to read injection template")
			return admission.Errored(http.StatusInternalServerError, err)
		}

		if tpl, err := template.New("sidecar").Parse(string(buf)); err == nil {
			var buf bytes.Buffer
			if err := tpl.Execute(&buf, sidecar); err == nil {
				decode := scheme.Codecs.UniversalDeserializer().Decode
				obj, _, _ := decode(buf.Bytes(), nil, nil)
				templatePod = obj.(*corev1.Pod)
			} else {
				logger.Error(err, "failed to execute template")
				return admission.Errored(http.StatusInternalServerError, err)
			}
		}
		r.ApplyToSidecar(sidecar, templatePod, pod)
	}

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		logger.Error(err, "failed to marshal pod")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	logger.Info("Injecting vineyard sidecar container successfully!")
	return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}

// ApplyToSidecar applies the sidecar cr and pod to the sidecar
func (r *Injector) ApplyToSidecar(sidecar *v1alpha1.Sidecar, pod *corev1.Pod, podWithSidecar *corev1.Pod) {
	// add sleep to wait for the sidecar container to be ready
	for i := range podWithSidecar.Spec.Containers {
		command := podWithSidecar.Spec.Containers[i].Command
		command[len(command)-1] = "while [ ! -e /var/run/vineyard.sock ]; do sleep 1; done;" + command[len(command)-1]
	}

	// add rpc labels to the podWithSidecar
	labels := podWithSidecar.Labels
	s := strings.Split(sidecar.Spec.Service.Selector, "=")
	// add the rpc label selector to the podWithSidecar's labels
	labels[s[0]] = s[1]

	// add volumeMounts to the app container
	if sidecar.Spec.Volume.PvcName == "" {
		// add emptyDir volumeMount for every app container
		for i := range podWithSidecar.Spec.Containers {
			podWithSidecar.Spec.Containers[i].VolumeMounts = append(podWithSidecar.Spec.Containers[i].VolumeMounts, corev1.VolumeMount{
				Name:      "vineyard-socket",
				MountPath: "/var/run",
			})
		}
	} else {
		// add pvc volumeMount for every app container
		for i := range podWithSidecar.Spec.Containers {
			podWithSidecar.Spec.Containers[i].VolumeMounts = append(podWithSidecar.Spec.Containers[i].VolumeMounts, corev1.VolumeMount{
				Name:      "vineyard-socket",
				MountPath: sidecar.Spec.Volume.MountPath,
			})
		}
	}
	// add the sidecar container
	podWithSidecar.Spec.Containers = append(podWithSidecar.Spec.Containers, pod.Spec.Containers...)
	podWithSidecar.Spec.Volumes = append(podWithSidecar.Spec.Volumes, pod.Spec.Volumes...)
}

// InjectDecoder injects the decoder.
func (r *Injector) InjectDecoder(d *admission.Decoder) error {
	r.decoder = d
	return nil
}
