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

package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-autoscaler/pkg/simulator/clustersnapshot"
	"sigs.k8s.io/cluster-autoscaler/pkg/simulator/clustersnapshot/testsnapshot"
	"sigs.k8s.io/cluster-autoscaler/pkg/simulator/drainability/rules"
	kube_util "sigs.k8s.io/cluster-autoscaler/pkg/utils/kubernetes"
	. "sigs.k8s.io/cluster-autoscaler/pkg/utils/test"
)

func withReplacementCPU(milli int64) func(*apiv1.Pod) {
	return func(pod *apiv1.Pod) {
		pod.Status.ResourceHints = append(pod.Status.ResourceHints, apiv1.PodResourceHint{
			Manager: "autoscaling.k8s.io/vpa-updater",
			Replacement: &apiv1.ResourceHintValues{Containers: []apiv1.ContainerResourceHint{{
				Name:     pod.Spec.Containers[0].Name,
				Requests: apiv1.ResourceList{apiv1.ResourceCPU: *resource.NewMilliQuantity(milli, resource.DecimalSI)},
			}}},
		})
	}
}

func TestSimulateNodeRemovalWithResourceHints(t *testing.T) {
	now := time.Now()
	replicas := int32(5)
	rsLister, err := kube_util.NewTestReplicaSetLister([]*appsv1.ReplicaSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"},
		Spec:       appsv1.ReplicaSetSpec{Replicas: &replicas},
	}})
	assert.NoError(t, err)
	registry := kube_util.NewListerRegistry(nil, nil, nil, nil, nil, nil, nil, rsLister, nil)
	ownerRefs := GenerateOwnerReferences("rs", "ReplicaSet", "extensions/v1beta1", "")

	rsPod := func(name string, milli int64, node string, opts ...func(*apiv1.Pod)) *apiv1.Pod {
		pod := BuildTestPod(name, milli, 100000, WithNodeName(node))
		pod.OwnerReferences = ownerRefs
		for _, o := range opts {
			o(pod)
		}
		return pod
	}

	for _, tc := range []struct {
		name          string
		hints         bool
		movedPod      *apiv1.Pod
		wantRemovable bool
		wantReason    UnremovableReason
	}{
		{
			name:          "replacement larger than free space, hints off: removable",
			movedPod:      rsPod("boosted", 200, "n1", withReplacementCPU(800)),
			wantRemovable: true,
		},
		{
			name:       "replacement larger than free space, hints on: no place to move",
			hints:      true,
			movedPod:   rsPod("boosted", 200, "n1", withReplacementCPU(800)),
			wantReason: NoPlaceToMovePods,
		},
		{
			name:       "replacement smaller than spec, hints off: no place to move",
			movedPod:   rsPod("shrunk", 800, "n1", withReplacementCPU(300)),
			wantReason: NoPlaceToMovePods,
		},
		{
			name:          "replacement smaller than spec, hints on: removable",
			hints:         true,
			movedPod:      rsPod("shrunk", 800, "n1", withReplacementCPU(300)),
			wantRemovable: true,
		},
		{
			name:          "no hints, hints on: removable (fail open)",
			hints:         true,
			movedPod:      rsPod("plain", 200, "n1"),
			wantRemovable: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n1 := BuildTestNode("n1", 1000, 2000000)
			n2 := BuildTestNode("n2", 1000, 2000000)
			SetNodeReadyState(n1, true, time.Time{})
			SetNodeReadyState(n2, true, time.Time{})
			nodes := []*apiv1.Node{n1, n2}
			pods := []*apiv1.Pod{tc.movedPod, rsPod("resident", 500, "n2")}

			opts := testDeleteOptions()
			opts.PodResourceHints = tc.hints
			snapshot := testsnapshot.NewTestSnapshotOrDie(t)
			clustersnapshot.InitializeClusterSnapshotOrDie(t, snapshot, nodes, pods)
			r := NewRemovalSimulator(registry, snapshot, opts, rules.Default(opts), false)
			toRemove, unremovable := r.SimulateNodeRemoval(context.Background(), "n1", map[string]bool{"n1": true, "n2": true}, now, nil)

			if tc.wantRemovable {
				assert.Nil(t, unremovable)
				if assert.NotNil(t, toRemove) {
					assert.Equal(t, []*apiv1.Pod{tc.movedPod}, toRemove.PodsToReschedule)
				}
				return
			}
			assert.Nil(t, toRemove)
			if assert.NotNil(t, unremovable) {
				assert.Equal(t, tc.wantReason, unremovable.Reason)
			}
		})
	}
}
