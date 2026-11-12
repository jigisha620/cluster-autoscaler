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
	"testing"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func rl(cpu, mem string) apiv1.ResourceList {
	l := apiv1.ResourceList{}
	if cpu != "" {
		l[apiv1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		l[apiv1.ResourceMemory] = resource.MustParse(mem)
	}
	return l
}

func ctr(name, cpu, mem string) apiv1.Container {
	return apiv1.Container{Name: name, Resources: apiv1.ResourceRequirements{Requests: rl(cpu, mem)}}
}

func vals(containers ...apiv1.ContainerResourceHint) *apiv1.ResourceHintValues {
	return &apiv1.ResourceHintValues{Containers: containers}
}

func ch(name, cpu, mem string) apiv1.ContainerResourceHint {
	return apiv1.ContainerResourceHint{Name: name, Requests: rl(cpu, mem)}
}

func assertList(t *testing.T, want, got apiv1.ResourceList) {
	t.Helper()
	for _, r := range []apiv1.ResourceName{apiv1.ResourceCPU, apiv1.ResourceMemory} {
		w, g := want[r], got[r]
		assert.Truef(t, w.Equal(g), "%s: want %s, got %s", r, w.String(), g.String())
	}
}

func TestHintedRequests(t *testing.T) {
	always := apiv1.ContainerRestartPolicyAlways
	for _, tc := range []struct {
		name string
		pod  apiv1.Pod
		want apiv1.ResourceList
	}{
		{
			name: "no hints keeps spec",
			pod:  apiv1.Pod{Spec: apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "1Gi")}}},
			want: rl("1", "1Gi"),
		},
		{
			name: "replacement merges per resource, unlisted resource keeps spec",
			pod: apiv1.Pod{
				Spec:   apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "1Gi")}},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: vals(ch("app", "3", ""))}}},
			},
			want: rl("3", "1Gi"),
		},
		{
			name: "replacement can shrink",
			pod: apiv1.Pod{
				Spec:   apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "3", "1Gi")}},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: vals(ch("app", "500m", ""))}}},
			},
			want: rl("500m", "1Gi"),
		},
		{
			name: "max across managers per container and resource",
			pod: apiv1.Pod{
				Spec: apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "1Gi"), ctr("side", "100m", "100Mi")}},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{
					{Manager: "a/vpa", Replacement: vals(ch("app", "2", "512Mi"))},
					{Manager: "b/other", Replacement: vals(ch("app", "1500m", "2Gi"), ch("side", "200m", ""))},
				}},
			},
			want: rl("2200m", "2148Mi"),
		},
		{
			name: "unknown container ignored",
			pod: apiv1.Pod{
				Spec:   apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "1Gi")}},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: vals(ch("gone", "8", ""))}}},
			},
			want: rl("1", "1Gi"),
		},
		{
			name: "replacement on native sidecar init container",
			pod: apiv1.Pod{
				Spec: apiv1.PodSpec{
					InitContainers: []apiv1.Container{func() apiv1.Container { c := ctr("proxy", "100m", ""); c.RestartPolicy = &always; return c }()},
					Containers:     []apiv1.Container{ctr("app", "1", "")},
				},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: vals(ch("proxy", "500m", ""))}}},
			},
			want: rl("1500m", ""),
		},
		{
			name: "replacement drops actuated status of a resized pod",
			pod: apiv1.Pod{
				Spec: apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "")}},
				Status: apiv1.PodStatus{
					ContainerStatuses: []apiv1.ContainerStatus{{Name: "app", AllocatedResources: rl("3", ""), Resources: &apiv1.ResourceRequirements{Requests: rl("3", "")}}},
					ResourceHints:     []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: vals(ch("app", "500m", ""))}},
				},
			},
			want: rl("500m", ""),
		},
		{
			name: "pod-level podRequests",
			pod: apiv1.Pod{
				Spec: apiv1.PodSpec{
					Resources:  &apiv1.ResourceRequirements{Requests: rl("2", "2Gi")},
					Containers: []apiv1.Container{ctr("app", "", "")},
				},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: &apiv1.ResourceHintValues{PodRequests: rl("4", "")}}}},
			},
			want: rl("4", "2Gi"),
		},
		{
			name: "podRequests without spec.resources ignored",
			pod: apiv1.Pod{
				Spec:   apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "")}},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: &apiv1.ResourceHintValues{PodRequests: rl("4", "")}}}},
			},
			want: rl("1", ""),
		},
		{
			name: "negative quantity ignored",
			pod: apiv1.Pod{
				Spec:   apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "")}},
				Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "a/vpa", Replacement: vals(ch("app", "-1", ""))}}},
			},
			want: rl("1", ""),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orig := tc.pod.DeepCopy()
			assertList(t, tc.want, HintedRequests(&tc.pod))
			assert.Equal(t, orig, &tc.pod, "input pod mutated")
		})
	}
}

func TestHintedPodAnnotatesManagers(t *testing.T) {
	pod := &apiv1.Pod{
		Spec: apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "")}},
		Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{
			{Manager: "a/vpa", Replacement: vals(ch("app", "2", ""))},
			{Manager: "b/empty"},
			{Manager: "c/other", Replacement: vals(ch("app", "3", ""))},
		}},
	}
	got, ok := HintedPod(pod)
	assert.True(t, ok)
	assert.Equal(t, "a/vpa,c/other", got.Annotations[HintsAppliedAnnotationKey])
	assert.Empty(t, pod.Annotations, "input pod mutated")
}

func TestHintedPodFailsOpen(t *testing.T) {
	pod := &apiv1.Pod{
		Spec:   apiv1.PodSpec{Containers: []apiv1.Container{ctr("app", "1", "")}},
		Status: apiv1.PodStatus{ResourceHints: []apiv1.PodResourceHint{{Manager: "b/empty"}}},
	}
	same, ok := HintedPod(pod)
	assert.False(t, ok)
	assert.Same(t, pod, same)
}
