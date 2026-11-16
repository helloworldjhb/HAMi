/*
Copyright 2026 The HAMi Authors.

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

package awsneuron

import (
	"maps"
	"strings"
	"testing"

	"gotest.tools/v3/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Project-HAMi/HAMi/pkg/device"
	"github.com/Project-HAMi/HAMi/pkg/device/common"
	"github.com/Project-HAMi/HAMi/pkg/util"
)

func TestInitContainerCoreMaskAccounting(t *testing.T) {
	oldDevices, oldSupport := device.DevicesMap, maps.Clone(device.SupportDevices)
	t.Cleanup(func() { device.DevicesMap, device.SupportDevices = oldDevices, oldSupport })
	device.DevicesMap = map[string]device.Devices{AWSNeuronDevice: InitAWSNeuronDevice(AWSNeuronConfig{
		ResourceCountName: "aws.amazon.com/neuron", ResourceCoreName: "aws.amazon.com/neuroncore",
	})}
	always := corev1.ContainerRestartPolicyAlways
	for _, tc := range []struct {
		name             string
		init             []corev1.Container
		masks            []int64
		reserved, steady int64
	}{
		{name: "disjoint init and app", init: []corev1.Container{{}}, masks: []int64{0b1100, 0b0001}, reserved: 0b1101, steady: 0b0001},
		{name: "overlapping init and app", init: []corev1.Container{{}}, masks: []int64{0b0011, 0b0001}, reserved: 0b0011, steady: 0b0001},
		{name: "sequential inits reuse cores", init: []corev1.Container{{}, {}}, masks: []int64{0b0011, 0b0011, 0b0001}, reserved: 0b0011, steady: 0b0001},
		{name: "sidecar before init", init: []corev1.Container{{RestartPolicy: &always}, {}}, masks: []int64{0b0001, 0b1100, 0b0010}, reserved: 0b1111, steady: 0b0011},
		{name: "sidecar after init reuses core", init: []corev1.Container{{}, {RestartPolicy: &always}}, masks: []int64{0b0011, 0b0001, 0b0010}, reserved: 0b0011, steady: 0b0011},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{InitContainers: tc.init, Containers: []corev1.Container{{}}}}
			rows := make(device.PodSingleDevice, len(tc.masks))
			for i, mask := range tc.masks {
				rows[i] = device.ContainerDevices{{UUID: "neuron-0", Type: AWSNeuronDevice, Usedcores: mask}}
			}
			raw := device.PodDevices{AWSNeuronDevice: rows}
			original := raw.DeepCopy()
			collapsed := device.CollapseInitContainerUsage(pod, raw)[AWSNeuronDevice]
			assert.Equal(t, collapsed[0][0].Usedcores, tc.reserved)
			steady := device.SteadyStateDeviceUsage(pod, raw)[AWSNeuronDevice]
			assert.Equal(t, steady[0][0].Usedcores, tc.steady)
			assert.DeepEqual(t, raw, original)
		})
	}
}

// A whole-device request must reserve every core exposed by the selected
// device, so no core request can share it afterwards.

func Test_GenerateResourceRequests_WholeDeviceDefersCoreCountToFit(t *testing.T) {
	dev := &AWSNeuronDevices{
		resourceCountName: "aws.amazon.com/neuron",
		resourceCoreName:  "aws.amazon.com/neuroncore",
	}
	ctr := &corev1.Container{
		Name: "ctr",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"aws.amazon.com/neuron": resource.MustParse("1"),
			},
		},
	}
	req, err := dev.GenerateResourceRequests(ctr)
	assert.NilError(t, err)
	assert.Equal(t, req.Coresreq, int64(0))
}

func Test_GetNodeDevices_RegistersFourCoreInferentia1Mask(t *testing.T) {
	dev := InitAWSNeuronDevice(AWSNeuronConfig{
		ResourceCountName: "aws.amazon.com/neuron",
		ResourceCoreName:  "aws.amazon.com/neuroncore",
	})
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "inf1-node",
			Labels: map[string]string{"node.kubernetes.io/instance-type": "inf1.6xlarge"},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				"aws.amazon.com/neuron":     resource.MustParse("4"),
				"aws.amazon.com/neuroncore": resource.MustParse("16"),
			},
		},
	}
	devices, err := dev.GetNodeDevices(node)
	assert.NilError(t, err)
	assert.Equal(t, len(devices), 4)
	assert.Equal(t, devices[0].Devcore, int64(15))
}

func Test_Fit_WholeDeviceNotShared(t *testing.T) {
	dev := &AWSNeuronDevices{
		resourceCountName: "aws.amazon.com/neuron",
		resourceCoreName:  "aws.amazon.com/neuroncore",
	}
	du := &device.DeviceUsage{
		ID:        "node-AWSNeuron-0",
		Index:     0,
		Count:     4,
		Totalcore: 15,
		Type:      AWSNeuronDevice,
		Health:    true,
		CustomInfo: map[string]any{
			AWSNodeType:             "inf1.6xlarge",
			AWSCoresPerNeuronDevice: int64(4),
		},
	}
	podA := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default", Annotations: map[string]string{}}}
	wholeDevice, err := dev.GenerateResourceRequests(&corev1.Container{
		Name: "ctr",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{"aws.amazon.com/neuron": resource.MustParse("1")},
		},
	})
	assert.NilError(t, err)
	fit, tmp, reason := dev.Fit([]*device.DeviceUsage{du}, wholeDevice, podA, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, true, reason)
	cd := tmp[AWSNeuronDevice][0]
	assert.NilError(t, dev.AddResourceUsage(podA, du, &cd))

	podB := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "default", Annotations: map[string]string{}}}
	coreReq := device.ContainerDeviceRequest{Nums: 1, Type: AWSNeuronDevice, Coresreq: 2}
	fitB, _, _ := dev.Fit([]*device.DeviceUsage{du}, coreReq, podB, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fitB, false)
}

func Test_Fit_FourCoreRequestNeedsEnoughDeviceCapacity(t *testing.T) {
	dev := InitAWSNeuronDevice(AWSNeuronConfig{
		ResourceCountName: "aws.amazon.com/neuron",
		ResourceCoreName:  "aws.amazon.com/neuroncore",
	})
	request, err := dev.GenerateResourceRequests(&corev1.Container{
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"aws.amazon.com/neuroncore": resource.MustParse("4"),
		}},
	})
	assert.NilError(t, err)
	assert.Equal(t, request.TotalCoresreq, int64(4))

	inf1Devices, err := dev.GetNodeDevices(newNeuronNode("inf1", "inf1.6xlarge", 1, 4))
	assert.NilError(t, err)
	inf1Usage := &device.DeviceUsage{
		ID:         inf1Devices[0].ID,
		Index:      inf1Devices[0].Index,
		Count:      inf1Devices[0].Count,
		Totalcore:  inf1Devices[0].Devcore,
		Type:       inf1Devices[0].Type,
		Health:     true,
		CustomInfo: inf1Devices[0].CustomInfo,
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "four-core", Namespace: "default", Annotations: map[string]string{}}}
	fit, allocations, reason := dev.Fit([]*device.DeviceUsage{inf1Usage}, request, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, true, reason)
	assert.Equal(t, allocations[AWSNeuronDevice][0].Usedcores, int64(15))

	inf2Devices, err := dev.GetNodeDevices(newNeuronNode("inf2", "inf2.8xlarge", 1, 2))
	assert.NilError(t, err)
	inf2Usage := &device.DeviceUsage{
		ID:         inf2Devices[0].ID,
		Index:      inf2Devices[0].Index,
		Count:      inf2Devices[0].Count,
		Totalcore:  inf2Devices[0].Devcore,
		Type:       inf2Devices[0].Type,
		Health:     true,
		CustomInfo: inf2Devices[0].CustomInfo,
	}
	fit, _, _ = dev.Fit([]*device.DeviceUsage{inf2Usage}, request, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
}

func Test_Fit_SharedCoresAreDistinctAndReplayable(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1", "inf1.6xlarge", 1, 4))
	assert.NilError(t, err)
	usage := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	replayed := usage.DeepCopy()
	request, err := dev.GenerateResourceRequests(&corev1.Container{Resources: corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"aws.amazon.com/neuroncore": resource.MustParse("1")},
	}})
	assert.NilError(t, err)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
	for _, wantMask := range []int64{1, 2, 4, 8} {
		fit, allocations, reason := dev.Fit([]*device.DeviceUsage{usage}, request, pod, &device.NodeInfo{}, &device.PodDevices{})
		assert.Equal(t, fit, true, reason)
		allocation := allocations[AWSNeuronDevice][0]
		assert.Equal(t, allocation.Usedcores, wantMask)
		assert.Equal(t, allocation.CustomInfo[AWSUsageInfo], int(wantMask))
		assert.NilError(t, dev.AddResourceUsage(pod, usage, &allocation))
		replayedAllocation := allocation
		replayedAllocation.CustomInfo = nil // Encoded Pod allocations do not retain CustomInfo.
		assert.NilError(t, dev.AddResourceUsage(pod, replayed, &replayedAllocation))
		assert.Equal(t, usage.Usedcores, replayed.Usedcores)
		assert.Equal(t, usage.CustomInfo[AWSUsageInfo], int(usage.Usedcores))
	}
	assert.Equal(t, usage.Usedcores, int64(15))
	fit, _, _ := dev.Fit([]*device.DeviceUsage{usage}, request, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
}

func Test_Fit_NeuronCoreRequestRequiresContiguousRange(t *testing.T) {
	dev := newNeuronBackend()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("2"),
			}},
		}}},
	}
	request, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	for _, test := range []struct {
		name     string
		used     int64
		fit      bool
		wantMask int64
		wantIDs  string
	}{
		// Core 1 is occupied. Cores 0 and 2 are free but cannot be combined;
		// Fit must choose the later contiguous range, 2-3.
		{name: "uses later contiguous range", used: 2, fit: true, wantMask: 12, wantIDs: "2,3"},
		{name: "rejects fragmented free cores", used: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := &device.DeviceUsage{
				ID: "neuron-0", Index: 0, Count: 4, Totalcore: 15,
				Usedcores: test.used, Type: AWSNeuronDevice, Health: true,
				CustomInfo: map[string]any{AWSCoresPerNeuronDevice: int64(4)},
			}
			fit, allocations, reason := dev.Fit([]*device.DeviceUsage{usage}, request, pod, &device.NodeInfo{}, &device.PodDevices{})
			assert.Equal(t, fit, test.fit, reason)
			if !test.fit {
				return
			}
			assert.Equal(t, allocations[AWSNeuronDevice][0].Usedcores, test.wantMask)
			annotations := map[string]string{util.AssignedNodeAnnotations: "inf1"}
			dev.PatchAnnotations(pod, &annotations, device.PodDevices{
				AWSNeuronDevice: device.PodSingleDevice{allocations[AWSNeuronDevice]},
			})
			assert.Equal(t, annotations[AWSNeuronAssignedIndex], test.wantIDs)
		})
	}
}

func Test_Fit_MultiDeviceCoreRequestUsesNodeGeometry(t *testing.T) {
	tests := []struct {
		name      string
		coresEach int64
		requested string
		wantMasks []int64
	}{
		{name: "four cores on two-core devices", coresEach: 2, requested: "4", wantMasks: []int64{3, 3}},
		{name: "six cores on two-core devices", coresEach: 2, requested: "6", wantMasks: []int64{3, 3, 3}},
		{name: "six cores on four-core devices", coresEach: 4, requested: "6", wantMasks: []int64{15, 3}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dev := newNeuronBackend()
			count := int64(len(test.wantMasks))
			registered, err := dev.GetNodeDevices(newNeuronNode("node", "inf2.8xlarge", count, count*test.coresEach))
			assert.NilError(t, err)
			usages := make([]*device.DeviceUsage, len(registered))
			for i, info := range registered {
				usages[i] = &device.DeviceUsage{
					ID: info.ID, Index: info.Index, Count: info.Count,
					Totalcore: info.Devcore, Type: info.Type, Health: true,
					CustomInfo: maps.Clone(info.CustomInfo),
				}
			}
			request, err := dev.GenerateResourceRequests(&corev1.Container{Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{"aws.amazon.com/neuroncore": resource.MustParse(test.requested)},
			}})
			assert.NilError(t, err)
			assert.Equal(t, request.Nums, int64(1))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}}
			fit, allocations, reason := dev.Fit(usages, request, pod, &device.NodeInfo{}, &device.PodDevices{})
			assert.Equal(t, fit, true, reason)
			assert.Equal(t, len(allocations[AWSNeuronDevice]), len(test.wantMasks))
			for i, want := range test.wantMasks {
				assert.Equal(t, allocations[AWSNeuronDevice][i].Usedcores, want)
			}
		})
	}
}

func Test_PatchAnnotations_EmitsAllInferentia1CoreIndexes(t *testing.T) {
	dev := InitAWSNeuronDevice(AWSNeuronConfig{
		ResourceCountName: "aws.amazon.com/neuron",
		ResourceCoreName:  "aws.amazon.com/neuroncore",
	})
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			"aws.amazon.com/neuroncore": resource.MustParse("4"),
		}},
	}}}}
	annotations := map[string]string{util.AssignedNodeAnnotations: "inf1"}
	dev.PatchAnnotations(pod, &annotations, device.PodDevices{
		AWSNeuronDevice: device.PodSingleDevice{device.ContainerDevices{{
			Idx:       1,
			Usedcores: 15,
			CustomInfo: map[string]any{
				AWSCoresPerNeuronDevice: int64(4),
			},
		}}},
	})
	assert.Equal(t, annotations[AWSNeuronAssignedIndex], "4,5,6,7")
}

func Test_Fit_MultiDeviceCoreRequestPermitsPartiallyOccupiedFinalDevice(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 2, 8))
	assert.NilError(t, err)
	assert.Equal(t, len(registered), 2)

	// Device 0: completely free (Used = 0, Usedcores = 0)
	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	// Device 1: 1 core allocated at core 3 (Used = 1, Usedcores = 8, free cores: 0, 1, 2)
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Used: 1, Usedcores: 8,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-5cores", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("5"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, allocations, reason := dev.Fit([]*device.DeviceUsage{dev0, dev1}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, true, reason)
	assert.Equal(t, len(allocations[AWSNeuronDevice]), 2)
	assert.Equal(t, allocations[AWSNeuronDevice][0].Idx, 0)
	assert.Equal(t, allocations[AWSNeuronDevice][0].Usedcores, int64(15))
	assert.Equal(t, allocations[AWSNeuronDevice][1].Idx, 1)
	assert.Equal(t, allocations[AWSNeuronDevice][1].Usedcores, int64(1))

	annotations := map[string]string{util.AssignedNodeAnnotations: "inf1-node"}
	dev.PatchAnnotations(pod, &annotations, device.PodDevices{
		AWSNeuronDevice: device.PodSingleDevice{allocations[AWSNeuronDevice]},
	})
	assert.Equal(t, annotations[AWSNeuronAssignedIndex], "0,1,2,3,4")
}

func Test_Fit_MultiDeviceCoreRequestPermitsPartiallyOccupiedFirstDevice(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 2, 8))
	assert.NilError(t, err)
	assert.Equal(t, len(registered), 2)

	// Device 0: lower cores 0 and 1 occupied (Used = 1, Usedcores = 3).
	// Top cores 2 and 3 are free (suffix).
	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Used: 1, Usedcores: 3,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	// Device 1: completely free (Used = 0, Usedcores = 0)
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-5cores", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("5"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, allocations, reason := dev.Fit([]*device.DeviceUsage{dev0, dev1}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, true, reason)
	assert.Equal(t, len(allocations[AWSNeuronDevice]), 2)
	// Dev0 allocates its top 2 cores (cores 2 and 3: mask 12 = 0b1100)
	assert.Equal(t, allocations[AWSNeuronDevice][0].Idx, 0)
	assert.Equal(t, allocations[AWSNeuronDevice][0].Usedcores, int64(12))
	// Dev1 allocates its bottom 3 cores (cores 0, 1, 2: mask 7 = 0b0111)
	assert.Equal(t, allocations[AWSNeuronDevice][1].Idx, 1)
	assert.Equal(t, allocations[AWSNeuronDevice][1].Usedcores, int64(7))

	annotations := map[string]string{util.AssignedNodeAnnotations: "inf1-node"}
	dev.PatchAnnotations(pod, &annotations, device.PodDevices{
		AWSNeuronDevice: device.PodSingleDevice{allocations[AWSNeuronDevice]},
	})
	// Global cores: Dev0 cores 2, 3 -> index 2, 3; Dev1 cores 0, 1, 2 -> index 4, 5, 6
	assert.Equal(t, annotations[AWSNeuronAssignedIndex], "2,3,4,5,6")
}

func Test_Fit_MultiDeviceCoreRequestRejectsFirstDeviceOccupiedTopCore(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 2, 8))
	assert.NilError(t, err)

	// Device 0: top core 3 is used (Used = 1, Usedcores = 8). Cores 0, 1, 2 are free.
	// Because core 3 is occupied, Dev0 cannot contribute its top cores across the device boundary.
	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Used: 1, Usedcores: 8,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	// Device 1: free
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-5cores", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("5"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, _, _ := dev.Fit([]*device.DeviceUsage{dev0, dev1}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
}

func Test_Fit_MultiDeviceCoreRequestRejectsNonContiguousBoundary(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 2, 8))
	assert.NilError(t, err)

	// Device 0: free
	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	// Device 1: core 0 is used (Used = 1, Usedcores = 1). Cores 1, 2, 3 are free (3 free cores).
	// Because core 0 (global core 4) is occupied, allocating 5 cores across dev0 and dev1
	// cannot form a contiguous range across the device boundary.
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Used: 1, Usedcores: 1,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-5cores", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("5"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, _, _ := dev.Fit([]*device.DeviceUsage{dev0, dev1}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
}

func Test_Fit_MultiDeviceCoreRequestRejectsPartiallyOccupiedIntermediateDevice(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 3, 12))
	assert.NilError(t, err)

	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	// Device 1 is intermediate for a 10-core request (4 on dev0, 4 on dev1, 2 on dev2).
	// Device 1 has 1 core used (Used = 1, Usedcores = 8), so it cannot satisfy 4 full cores.
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Used: 1, Usedcores: 8,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}
	dev2 := &device.DeviceUsage{
		ID: registered[2].ID, Index: registered[2].Index,
		Count: registered[2].Count, Totalcore: registered[2].Devcore,
		Type: registered[2].Type, Health: true,
		CustomInfo: maps.Clone(registered[2].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-10cores", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("10"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, _, _ := dev.Fit([]*device.DeviceUsage{dev0, dev1, dev2}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
}

func Test_Fit_MultiDeviceCoreRequestHonorsMutexPolicy(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 2, 8))
	assert.NilError(t, err)

	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Used: 1, Usedcores: 8,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "workload-mutex",
			Namespace:   "default",
			Annotations: map[string]string{"hami.io/gpu-scheduler-policy": "mutex"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuroncore": resource.MustParse("5"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, _, reason := dev.Fit([]*device.DeviceUsage{dev0, dev1}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
	assert.Assert(t, strings.Contains(reason, common.NumaNotFit), "expected %s in reason: %s", common.NumaNotFit, reason)
}

func Test_Fit_MultiDeviceWholeDeviceRequestRejectsPartiallyOccupiedDevice(t *testing.T) {
	dev := newNeuronBackend()
	registered, err := dev.GetNodeDevices(newNeuronNode("inf1-node", "inf1.6xlarge", 2, 8))
	assert.NilError(t, err)

	dev0 := &device.DeviceUsage{
		ID: registered[0].ID, Index: registered[0].Index,
		Count: registered[0].Count, Totalcore: registered[0].Devcore,
		Type: registered[0].Type, Health: true,
		CustomInfo: maps.Clone(registered[0].CustomInfo),
	}
	dev1 := &device.DeviceUsage{
		ID: registered[1].ID, Index: registered[1].Index,
		Count: registered[1].Count, Totalcore: registered[1].Devcore,
		Used: 1, Usedcores: 8,
		Type: registered[1].Type, Health: true,
		CustomInfo: maps.Clone(registered[1].CustomInfo),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload-2devices", Namespace: "default", Annotations: map[string]string{}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"aws.amazon.com/neuron": resource.MustParse("2"),
			}},
		}}},
	}
	req, err := dev.GenerateResourceRequests(&pod.Spec.Containers[0])
	assert.NilError(t, err)

	fit, _, reason := dev.Fit([]*device.DeviceUsage{dev0, dev1}, req, pod, &device.NodeInfo{}, &device.PodDevices{})
	assert.Equal(t, fit, false)
	assert.Assert(t, strings.Contains(reason, common.NumaNotFit), "expected %s in reason: %s", common.NumaNotFit, reason)
}
