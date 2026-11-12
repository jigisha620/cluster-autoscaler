/*
Copyright 2026 The Kubernetes Authors.

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

package pod

import (
	"sort"
	"strings"

	apiv1 "k8s.io/api/core/v1"
)

// HintsAppliedAnnotationKey is set on pod copies rewritten from hints. The value lists the managers whose hints were used.
const HintsAppliedAnnotationKey = "cluster-autoscaler.kubernetes.io/resource-hints-applied"

// MergedHint combines the replacement hints of all managers on the pod, taking the per-container,
// per-resource max. Entries naming unknown containers, and negative quantities, are ignored. It returns nil
// and no managers when no manager sets a usable value (KEP C5, fail open).
func MergedHint(pod *apiv1.Pod) (*apiv1.ResourceHintValues, []string) {
	if pod == nil || len(pod.Status.ResourceHints) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	for _, c := range pod.Spec.Containers {
		known[c.Name] = true
	}
	for _, c := range pod.Spec.InitContainers {
		known[c.Name] = true
	}
	containers := map[string]apiv1.ResourceList{}
	var podRequests apiv1.ResourceList
	var managers []string
	for i := range pod.Status.ResourceHints {
		h := &pod.Status.ResourceHints[i]
		v := h.Replacement
		if v == nil {
			continue
		}
		used := false
		for _, c := range v.Containers {
			if !known[c.Name] {
				continue
			}
			for r, q := range c.Requests {
				if q.Sign() < 0 {
					continue
				}
				if containers[c.Name] == nil {
					containers[c.Name] = apiv1.ResourceList{}
				}
				if cur, ok := containers[c.Name][r]; !ok || q.Cmp(cur) > 0 {
					containers[c.Name][r] = q.DeepCopy()
				}
				used = true
			}
		}
		if pod.Spec.Resources != nil {
			for r, q := range v.PodRequests {
				if q.Sign() < 0 {
					continue
				}
				if podRequests == nil {
					podRequests = apiv1.ResourceList{}
				}
				if cur, ok := podRequests[r]; !ok || q.Cmp(cur) > 0 {
					podRequests[r] = q.DeepCopy()
				}
				used = true
			}
		}
		if used {
			managers = append(managers, h.Manager)
		}
	}
	if len(managers) == 0 {
		return nil, nil
	}
	merged := &apiv1.ResourceHintValues{PodRequests: podRequests}
	names := make([]string, 0, len(containers))
	for n := range containers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		merged.Containers = append(merged.Containers, apiv1.ContainerResourceHint{Name: n, Requests: containers[n]})
	}
	return merged, managers
}

// HintedPod returns a deep copy of pod with its merged replacement hint applied to its requests, and true.
// If no manager sets a usable replacement, it returns pod unchanged and false.
//
// The copy models a new pod (KEP C1): hinted container and pod-level requests replace the spec values, and
// status resources and resize conditions are dropped so the resource helper reads the spec value. The copy
// is annotated with the managers whose hints were used (KEP C6).
func HintedPod(pod *apiv1.Pod) (*apiv1.Pod, bool) {
	hint, managers := MergedHint(pod)
	if hint == nil {
		return pod, false
	}
	byName := map[string]apiv1.ResourceList{}
	for _, c := range hint.Containers {
		byName[c.Name] = c.Requests
	}
	out := pod.DeepCopy()
	apply := func(c *apiv1.Container) {
		hinted := byName[c.Name]
		if len(hinted) == 0 {
			return
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = apiv1.ResourceList{}
		}
		for r, q := range hinted {
			c.Resources.Requests[r] = q.DeepCopy()
		}
	}
	for i := range out.Spec.Containers {
		apply(&out.Spec.Containers[i])
	}
	for i := range out.Spec.InitContainers {
		apply(&out.Spec.InitContainers[i])
	}
	if out.Spec.Resources != nil && len(hint.PodRequests) > 0 {
		if out.Spec.Resources.Requests == nil {
			out.Spec.Resources.Requests = apiv1.ResourceList{}
		}
		for r, q := range hint.PodRequests {
			out.Spec.Resources.Requests[r] = q.DeepCopy()
		}
	}
	for i := range out.Status.ContainerStatuses {
		out.Status.ContainerStatuses[i].Resources = nil
		out.Status.ContainerStatuses[i].AllocatedResources = nil
	}
	for i := range out.Status.InitContainerStatuses {
		out.Status.InitContainerStatuses[i].Resources = nil
		out.Status.InitContainerStatuses[i].AllocatedResources = nil
	}
	out.Status.Resources = nil
	out.Status.AllocatedResources = nil
	conds := out.Status.Conditions[:0]
	for _, c := range out.Status.Conditions {
		if c.Type == apiv1.PodResizePending || c.Type == apiv1.PodResizeInProgress {
			continue
		}
		conds = append(conds, c)
	}
	out.Status.Conditions = conds
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	out.Annotations[HintsAppliedAnnotationKey] = strings.Join(managers, ",")
	return out, true
}

// HintedRequests returns the pod's requests with its replacement hint applied, as computed by PodRequests.
// Without a usable hint it equals PodRequests(pod).
func HintedRequests(pod *apiv1.Pod) apiv1.ResourceList {
	hinted, _ := HintedPod(pod)
	return PodRequests(hinted)
}

// HintedPods applies HintedPod to each pod. The returned slice shares unhinted pods with the input.
func HintedPods(pods []*apiv1.Pod) []*apiv1.Pod {
	out := make([]*apiv1.Pod, 0, len(pods))
	for _, p := range pods {
		hp, _ := HintedPod(p)
		out = append(out, hp)
	}
	return out
}
