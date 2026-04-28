//go:build e2e

/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package e2e

import (
	"context"
	"fmt"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/dynamic-resource-allocation/api/metadata"
	gpuv1alpha1 "sigs.k8s.io/dra-example-driver/api/example.com/resource/gpu/v1alpha1"
)

var _ = Describe("Test GPU allocation", func() {
	It("should allocate 1 distinct GPU per pod", func(ctx SpecContext) {
		namespace := "basic-resourceclaimtemplate"
		pods := []string{"pod0", "pod1"}
		containerName := "ctr0"
		expectedGPUCount := 1

		deployManifest(ctx, namespace, "basic-resourceclaimtemplate.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		observedGPUs := make(map[string]string)
		for _, podName := range pods {
			verifyGPUAllocation(ctx, namespace, podName, containerName, expectedGPUCount, observedGPUs)
		}
	})

	It("should allocate 2 distinct GPUs to a single container", func(ctx SpecContext) {
		namespace := "basic-multiple-requests"
		pods := []string{"pod0"}
		containerName := "ctr0"
		expectedGPUCount := 2

		deployManifest(ctx, namespace, "basic-multiple-requests.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		observedGPUs := make(map[string]string)
		verifyGPUAllocation(ctx, namespace, pods[0], containerName, expectedGPUCount, observedGPUs)
	})

	It("should share 1 GPU between containers with TimeSlicing default interval", func(ctx SpecContext) {
		namespace := "basic-shared-claim-across-containers"
		pods := []string{"pod0"}

		deployManifest(ctx, namespace, "basic-shared-claim-across-containers.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		verifySharedGPUGroup(ctx, namespace, sharingGroup{
			members: []podContainer{
				{pod: "pod0", container: "ctr0"},
				{pod: "pod0", container: "ctr1"},
			},
			expectedStrategy:  string(gpuv1alpha1.TimeSlicingStrategy),
			expectedProperty:  "TIMESLICE_INTERVAL",
			expectedPropValue: string(gpuv1alpha1.DefaultTimeSlice),
		})
	})

	It("should share 1 GPU between pods with TimeSlicing default interval", func(ctx SpecContext) {
		namespace := "basic-shared-claim-across-pods"
		pods := []string{"pod0", "pod1"}

		deployManifest(ctx, namespace, "basic-shared-claim-across-pods.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		verifySharedGPUGroup(ctx, namespace, sharingGroup{
			members: []podContainer{
				{pod: "pod0", container: "ctr0"},
				{pod: "pod1", container: "ctr0"},
			},
			expectedStrategy:  string(gpuv1alpha1.TimeSlicingStrategy),
			expectedProperty:  "TIMESLICE_INTERVAL",
			expectedPropValue: string(gpuv1alpha1.DefaultTimeSlice),
		})
	})

	It("should share GPUs with TimeSlicing and SpacePartitioning", func(ctx SpecContext) {
		namespace := "basic-resourceclaim-opaque-config"
		pods := []string{"pod0"}

		deployManifest(ctx, namespace, "basic-resourceclaim-opaque-config.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		verifySharedGPUGroup(ctx, namespace, sharingGroup{
			members: []podContainer{
				{pod: "pod0", container: "ts-ctr0"},
				{pod: "pod0", container: "ts-ctr1"},
			},
			expectedStrategy:  string(gpuv1alpha1.TimeSlicingStrategy),
			expectedProperty:  "TIMESLICE_INTERVAL",
			expectedPropValue: string(gpuv1alpha1.LongTimeSlice),
		})

		verifySharedGPUGroup(ctx, namespace, sharingGroup{
			members: []podContainer{
				{pod: "pod0", container: "sp-ctr0"},
				{pod: "pod0", container: "sp-ctr1"},
			},
			expectedStrategy:  string(gpuv1alpha1.SpacePartitioningStrategy),
			expectedProperty:  "PARTITION_COUNT",
			expectedPropValue: "10",
		})
	})

	It("should share 1 GPU between init container and regular container", func(ctx SpecContext) {
		namespace := "initcontainer-shared-gpu"
		pods := []string{"pod0"}

		deployManifest(ctx, namespace, "initcontainer-shared-gpu.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		verifySharedGPUGroup(ctx, namespace, sharingGroup{
			members: []podContainer{
				{pod: "pod0", container: "init0"},
				{pod: "pod0", container: "ctr0"},
			},
			expectedStrategy:  string(gpuv1alpha1.TimeSlicingStrategy),
			expectedProperty:  "TIMESLICE_INTERVAL",
			expectedPropValue: string(gpuv1alpha1.DefaultTimeSlice),
		})
	})

	It("should have DRA_ADMIN_ACCESS set to true", func(ctx SpecContext) {
		namespace := "admin-access"
		pods := []string{"pod0"}
		containerName := "ctr0"

		deployManifest(ctx, namespace, "admin-access.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)
		verifyDRAAdminAccess(ctx, namespace, pods[0], containerName, "true")
	})

	It("should allocate 1 GPU selected using CEL expression", func(ctx SpecContext) {
		namespace := "cel-selector"
		pods := []string{"pod0"}
		containerName := "ctr0"
		expectedGPUCount := 1

		deployManifest(ctx, namespace, "cel-selector.yaml")
		checkPodsReadyAndRunning(ctx, namespace, pods)

		observedGPUs := make(map[string]string)
		verifyGPUAllocation(ctx, namespace, pods[0], containerName, expectedGPUCount, observedGPUs)
	})

	It("Should share 1 GPU among the Pods in each of 2 PodGroups", func(ctx SpecContext) {
		namespace := "podgroup-resourceclaimtemplate"
		containerName := "ctr0"
		expectedGPUCount := 1

		deployManifest(ctx, namespace, "podgroup-resourceclaimtemplate.yaml")

		Eventually(ctx, func(g Gomega, ctx context.Context) {
			deployments, err := clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			for _, deployment := range deployments.Items {
				g.Expect(deployment.Status.ObservedGeneration).To(Equal(deployment.Generation))
				g.Expect(deployment.Status.Replicas).To(Equal(*deployment.Spec.Replicas))
			}
		}, "30s", "1s").Should(Succeed())

		pods := map[string][]corev1.Pod{} // PodGroup name -> Pods
		podList, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		Expect(err).NotTo(HaveOccurred())

		var podNames []string
		for _, pod := range podList.Items {
			pg := *pod.Spec.SchedulingGroup.PodGroupName
			pods[pg] = append(pods[pg], pod)
			podNames = append(podNames, pod.Name)
		}

		checkPodsReadyAndRunning(ctx, namespace, podNames)

		observedGPUs := make(map[string]string)
		for _, groupPods := range pods {
			for _, pod := range groupPods {
				verifyGPUAllocation(ctx, namespace, pod.Name, containerName, expectedGPUCount, observedGPUs)
				break // only check one Pod for each PodGroup
			}
		}

		for _, groupPods := range pods {
			var members []podContainer
			for _, pod := range groupPods {
				members = append(members, podContainer{pod: pod.Name, container: containerName})
			}
			verifySharedGPUGroup(ctx, namespace, sharingGroup{
				members:           members,
				expectedStrategy:  string(gpuv1alpha1.TimeSlicingStrategy),
				expectedProperty:  "TIMESLICE_INTERVAL",
				expectedPropValue: string(gpuv1alpha1.DefaultTimeSlice),
			})
		}
	})

	Context("GPU Test 9- One pod, one container, verify in-container device metadata for ResourceClaimTemplate", func() {
		namespace := "gpu-test9"
		pods := []string{"pod0"}
		containerName := "ctr0"
		expectedGPUCount := 1

		It("should have all the pods ready and running", func() {
			checkPodsReadyAndRunning(namespace, pods, len(pods))
		})
		It("should have exactly 1 GPU with valid device metadata", func() {
			var gpus []string
			Eventually(func(g Gomega) {
				By("checking that there is exactly 1 GPU")
				gpus, _ = getGPUsFromPodLogs(namespace, pods[0], containerName)
				verifyGPUCount(g, gpus, expectedGPUCount, namespace, pods[0], containerName)
				claimNewGPU(g, gpus[0], namespace, pods[0], containerName)
			}, checkPodLogsTimeout, checkPodLogsInterval).Should(Succeed())

			metadataPath := metadata.ResourceClaimTemplateFilePath("gpu.example.com", "gpu", "gpu")
			By("checking that the device metadata file exists and is valid")
			dm, err := readDeviceMetadata(namespace, pods[0], containerName, metadataPath)
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Expected to read metadata file at %s", metadataPath))
			fmt.Fprintf(GinkgoWriter, "Pod %s/%s, container %s has metadata file at: %s\n",
				namespace, pods[0], containerName, metadataPath)
			Expect(dm.PodClaimName).ToNot(BeEmpty(), "Expected PodClaimName in metadata to not be empty")
			Expect(dm.Requests).To(HaveLen(1))
		})
	})
	Context("GPU Test 10- One pod, one container, verify in-container device metadata for ResourceClaim", func() {
		namespace := "gpu-test10"
		pods := []string{"pod0"}
		containerName := "ctr0"
		expectedGPUCount := 1

		It("should have all the pods ready and running", func() {
			checkPodsReadyAndRunning(namespace, pods, len(pods))
		})
		It("should have exactly 1 GPU with valid device metadata", func() {
			var gpus []string
			Eventually(func(g Gomega) {
				By("checking that there is exactly 1 GPU")
				gpus, _ = getGPUsFromPodLogs(namespace, pods[0], containerName)
				verifyGPUCount(g, gpus, expectedGPUCount, namespace, pods[0], containerName)
				claimNewGPU(g, gpus[0], namespace, pods[0], containerName)
			}, checkPodLogsTimeout, checkPodLogsInterval).Should(Succeed())

			metadataPath := metadata.ResourceClaimFilePath("gpu.example.com", "single-gpu", "gpu")
			By("checking that the device metadata file exists and is valid")
			dm, err := readDeviceMetadata(namespace, pods[0], containerName, metadataPath)
			Expect(err).NotTo(HaveOccurred(),
				fmt.Sprintf("Expected to read metadata file at %s", metadataPath))
			fmt.Fprintf(GinkgoWriter, "Pod %s/%s, container %s has metadata file at: %s\n",
				namespace, pods[0], containerName, metadataPath)
			Expect(dm.Requests).To(HaveLen(1))
		})
	})
	Context("Webhooks", func() {
		tests := []struct {
			name     string
			fileName string
		}{
			{name: "v1 ResourceClaim", fileName: "invalid_rc_v1.yaml"},
			{name: "v1beta1 ResourceClaim", fileName: "invalid_rc_v1beta1.yaml"},
			{name: "v1 ResourceClaimTemplate", fileName: "invalid_rc_template.yaml"},
		}

		for _, testCase := range tests {
			It("should reject invalid "+testCase.name, func(ctx SpecContext) {
				manifestPath := filepath.Join(currentDir, "testdata", "webhooks", testCase.fileName)

				err := createManifestWithDryRun(ctx, dynamicClient, manifestPath)
				fmt.Fprintf(GinkgoWriter, "Error from create: %v\n", err)
				Expect(err).To(HaveOccurred(),
					"Expected webhook to reject %s, but it was accepted", testCase.fileName)
				Expect(err.Error()).To(ContainSubstring("unknown time-slice interval"),
					"Webhook did not reject %s invalid GpuConfig with the expected message. Got error: %v", testCase.name, err)
			})
		}
	})
})
