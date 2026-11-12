//go:build e2e

/*
Copyright The Kubernetes Authors.

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

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// These tests need a cluster with the PodResourceHints feature gate and CA running with
// --pod-resource-hints=true. Set CA_E2E_POD_RESOURCE_HINTS=true to run them.
//
// Only replacement is tested (KEP C1). The earlier cases for infeasible target grow (old case 3) and
// pressure stalls (old case 4, with fail open as case 5) were removed with those fields from the KEP.

const hintsManager = "e2e.cluster-autoscaler.kubernetes.io/hints"

func skipUnlessHints(t *testing.T) {
	if os.Getenv("CA_E2E_POD_RESOURCE_HINTS") != "true" {
		t.Skip("CA_E2E_POD_RESOURCE_HINTS != true")
	}
}

func hintValues(container, cpu string) map[string]any {
	return map[string]any{"containers": []any{map[string]any{"name": container, "requests": map[string]string{"cpu": cpu}}}}
}

// applyHint server-side applies one manager's entry through the pods/resourcehints subresource.
func applyHint(ctx context.Context, cfg *envconf.Config, pod *corev1.Pod, hint map[string]any) error {
	cs, err := kubernetes.NewForConfig(cfg.Client().RESTConfig())
	if err != nil {
		return err
	}
	hint["manager"] = hintsManager
	hint["updateTime"] = time.Now().UTC().Format(time.RFC3339)
	body, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": pod.Name, "namespace": pod.Namespace},
		"status":   map[string]any{"resourceHints": []any{hint}},
	})
	if err != nil {
		return err
	}
	force := true
	return cs.CoreV1().RESTClient().Patch(types.ApplyPatchType).Namespace(pod.Namespace).Resource("pods").Name(pod.Name).
		SubResource("resourcehints").VersionedParams(&metav1.PatchOptions{FieldManager: hintsManager, Force: &force}, metav1.ParameterCodec).
		Body(body).Do(ctx).Error()
}

func safeToEvict(pod *corev1.Pod) *corev1.Pod {
	pod.Annotations = map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict": "true"}
	return pod
}

func nodeOf(ctx context.Context, t *testing.T, client klient.Client, pod *corev1.Pod) string {
	t.Helper()
	got := &corev1.Pod{}
	if err := client.Resources().Get(ctx, pod.Name, pod.Namespace, got); err != nil {
		t.Fatalf("get pod %s: %v", pod.Name, err)
	}
	return got.Spec.NodeName
}

func mustCreateScheduled(ctx context.Context, t *testing.T, client klient.Client, pod *corev1.Pod) {
	t.Helper()
	if err := client.Resources().Create(ctx, pod); err != nil {
		t.Fatalf("create pod %s: %v", pod.Name, err)
	}
	if err := WaitForPodScheduled(ctx, client, pod, testCfg.PodSchedulingTimeout); err != nil {
		t.Fatalf("pod %s not scheduled: %v", pod.Name, err)
	}
}

// Case 1 (scale-down churn): a pod whose replacement does not fit the remaining nodes keeps its node.
func TestResourceHintsReplacementBlocksScaleDown(t *testing.T) {
	skipUnlessHints(t)
	ns := testEnv.EnvConf().Namespace()
	filler := NewTestPodWithResourceFraction("hints-filler", ns, 0.7, 0.02)
	blocker := NewTestPodWithResourceFraction("hints-blocker", ns, 0.5, 0.02)
	mover := safeToEvict(NewTestPodWithResourceFraction("hints-mover", ns, 0.2, 0.02))

	feature := features.New("Replacement hint blocks scale-down").
		Assess("replacement larger than free space keeps the node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatal(err)
			}
			mustCreateScheduled(ctx, t, client, filler)
			mustCreateScheduled(ctx, t, client, blocker)
			if err := WaitForNodesReady(ctx, client, testCfg.NodeGroup, 2, testCfg.NodeReadyTimeout); err != nil {
				t.Fatalf("expected 2 nodes: %v", err)
			}
			mover.Spec.NodeName = nodeOf(ctx, t, client, blocker)
			mustCreateScheduled(ctx, t, client, mover)
			if err := applyHint(ctx, cfg, mover, map[string]any{"replacement": hintValues("fake-container", testCfg.CalculateCPURequest(0.5))}); err != nil {
				t.Fatalf("apply hint: %v", err)
			}
			if err := client.Resources().Delete(ctx, blocker); err != nil {
				t.Fatalf("delete blocker: %v", err)
			}
			_ = WaitForPodDeleted(ctx, client, blocker, testCfg.PodDeletionTimeout)
			if err := WaitForNodeCountConsistently(ctx, client, testCfg.NodeGroup, 2, testCfg.NoScaleDownWindow); err != nil {
				t.Fatalf("node removed despite large replacement: %v", err)
			}
			if err := applyHint(ctx, cfg, mover, map[string]any{"replacement": hintValues("fake-container", testCfg.CalculateCPURequest(0.2))}); err != nil {
				t.Fatalf("apply hint: %v", err)
			}
			if err := WaitForNodeCount(ctx, client, testCfg.NodeGroup, 1, testCfg.ScaleDownTimeout); err != nil {
				t.Fatalf("node kept after replacement shrank: %v", err)
			}
			return ctx
		}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			client, err := cfg.NewClient()
			if err != nil {
				t.Fatal(err)
			}
			TeardownPodAndNodeGroup(ctx, client, []*corev1.Pod{filler, blocker, mover}, testCfg.NodeGroup)
			return ctx
		}).
		Feature()

	testEnv.Test(t, feature)
}
