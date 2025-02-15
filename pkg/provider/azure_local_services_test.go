/*
Copyright 2023 The Kubernetes Authors.

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

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"

	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/cloud-provider-azure/pkg/azureclients/loadbalancerclient/mockloadbalancerclient"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
	"sigs.k8s.io/cloud-provider-azure/pkg/retry"
)

func TestLoadBalancerBackendPoolUpdater(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	addOperationPool1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	removeOperationPool1 := getRemoveIPsFromBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})
	addOperationPool2 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"})

	testCases := []struct {
		name                               string
		operations                         []batchOperation
		existingBackendPools               []network.BackendAddressPool
		expectedGetBackendPool             network.BackendAddressPool
		extraWait                          bool
		notLocal                           bool
		changeLB                           bool
		removeOperationServiceName         string
		expectedCreateOrUpdateBackendPools []network.BackendAddressPool
		expectedBackendPools               []network.BackendAddressPool
	}{
		{
			name:       "Add node IPs to backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name:       "Remove node IPs from backend pool",
			operations: []batchOperation{addOperationPool1, removeOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:       "Multiple operations targeting different backend pools",
			operations: []batchOperation{addOperationPool1, addOperationPool2, removeOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
				getTestBackendAddressPoolWithIPs("lb1", "pool2", []string{"10.0.0.1", "10.0.0.2"}),
			},
		},
		{
			name:       "Multiple operations in two batches",
			operations: []batchOperation{addOperationPool1, removeOperationPool1},
			extraWait:  true,
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
		},
		{
			name:                       "remove operations by service name",
			operations:                 []batchOperation{addOperationPool1, removeOperationPool1},
			removeOperationServiceName: "ns1/svc1",
		},
		{
			name:       "not local service",
			operations: []batchOperation{addOperationPool1},
			notLocal:   true,
		},
		{
			name:       "not on this load balancer",
			operations: []batchOperation{addOperationPool1},
			changeLB:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			if !tc.notLocal {
				cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})
			}
			if tc.changeLB {
				cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb2"})
			}
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
			if len(tc.existingBackendPools) > 0 {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[0].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[0], nil)
			}
			if len(tc.existingBackendPools) == 2 {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[1].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[1], nil)
			}
			if tc.extraWait {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedGetBackendPool.Name,
					gomock.Any(),
				).Return(tc.expectedGetBackendPool, nil)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) > 0 {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[0].Name,
					tc.expectedCreateOrUpdateBackendPools[0],
					gomock.Any(),
				).Return(nil)
			}
			if len(tc.existingBackendPools) == 2 || tc.extraWait {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[1].Name,
					tc.expectedCreateOrUpdateBackendPools[1],
					gomock.Any(),
				).Return(nil)
			}
			cloud.LoadBalancerClient = mockLBClient

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go u.run(ctx)

			results := sync.Map{}
			for _, op := range tc.operations {
				op := op
				go func() {
					u.addOperation(op)
					result := op.wait()
					results.Store(result, true)
				}()
				time.Sleep(100 * time.Millisecond)
				if tc.extraWait {
					time.Sleep(time.Second)
				}
			}
			if tc.removeOperationServiceName != "" {
				u.removeOperation(tc.removeOperationServiceName)
			}
			time.Sleep(3 * time.Second)
			//err := wait.PollUntilContextTimeout(context.Background(), time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			//	resultLen := 0
			//	results.Range(func(key, value interface{}) bool {
			//		resultLen++
			//		return true
			//	})
			//	return resultLen == len(tc.expectedResult), nil
			//})
			//assert.NoError(t, err)

			//actualResults := make(map[batchOperationResult]bool)
			//results.Range(func(key, value interface{}) bool {
			//	actualResults[key.(batchOperationResult)] = true
			//	return true
			//})

			//err = wait.PollUntilContextTimeout(ctx, time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			//	return assert.Equal(t, tc.expectedResult, actualResults, tc.name), nil
			//})
			//assert.NoError(t, err)
		})
	}
}

func TestLoadBalancerBackendPoolUpdaterFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	addOperationPool1 := getAddIPsToBackendPoolOperation("ns1/svc1", "lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"})

	testCases := []struct {
		name                               string
		operations                         []batchOperation
		existingBackendPools               []network.BackendAddressPool
		expectedGetBackendPool             network.BackendAddressPool
		getBackendPoolErr                  *retry.Error
		putBackendPoolErr                  *retry.Error
		expectedCreateOrUpdateBackendPools []network.BackendAddressPool
		expectedBackendPools               []network.BackendAddressPool
		//expectedResult                     map[batchOperationResult]bool
		//expectedPoolNameToErrMsg           map[string]string
		//expectedResultCount                int
	}{
		{
			name:       "Retriable error when getting backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: retry.NewError(true, errors.New("error")),
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			//expectedResult: map[batchOperationResult]bool{
			//	newBatchOperationResult("lb1/pool1", true, nil): true,
			//},
			//expectedResultCount: 1,
		},
		{
			name:       "Retriable error when updating backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			putBackendPoolErr:      retry.NewError(true, errors.New("error")),
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			//expectedResult: map[batchOperationResult]bool{
			//	newBatchOperationResult("lb1/pool1", true, nil): true,
			//},
			//expectedResultCount: 1,
		},
		{
			name:       "Non-retriable error when getting backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: retry.NewError(false, fmt.Errorf("error")),
			expectedBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			//expectedPoolNameToErrMsg: map[string]string{"lb1/pool1": "Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"},
			//expectedResultCount:      1,
		},
		{
			name:       "Non-retriable error when updating backend pool",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			expectedGetBackendPool: getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			putBackendPoolErr:      retry.NewError(false, fmt.Errorf("error")),
			expectedCreateOrUpdateBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{"10.0.0.1", "10.0.0.2"}),
			},
			//expectedPoolNameToErrMsg: map[string]string{"lb1/pool1": "Retriable: false, RetryAfter: 0s, HTTPStatusCode: 0, RawError: error"},
			//expectedResultCount:      1,
		},
		{
			name:       "Backend pool not found",
			operations: []batchOperation{addOperationPool1},
			existingBackendPools: []network.BackendAddressPool{
				getTestBackendAddressPoolWithIPs("lb1", "pool1", []string{}),
			},
			getBackendPoolErr: &retry.Error{
				HTTPStatusCode: http.StatusNotFound,
				Retriable:      false,
				RawError:       errors.New("error"),
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			cloud.localServiceNameToServiceInfoMap.Store("ns1/svc1", &serviceInfo{lbName: "lb1"})
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
			mockLBClient.EXPECT().GetLBBackendPool(
				gomock.Any(),
				gomock.Any(),
				"lb1",
				*tc.existingBackendPools[0].Name,
				gomock.Any(),
			).Return(tc.existingBackendPools[0], tc.getBackendPoolErr)
			if tc.getBackendPoolErr != nil && tc.getBackendPoolErr.Retriable {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[0].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[0], nil)
			}
			if len(tc.existingBackendPools) == 2 {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.existingBackendPools[1].Name,
					gomock.Any(),
				).Return(tc.existingBackendPools[1], nil)
			}
			if tc.putBackendPoolErr != nil && tc.putBackendPoolErr.Retriable {
				mockLBClient.EXPECT().GetLBBackendPool(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedGetBackendPool.Name,
					gomock.Any(),
				).Return(tc.expectedGetBackendPool, nil)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) > 0 {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[0].Name,
					tc.expectedCreateOrUpdateBackendPools[0],
					gomock.Any(),
				).Return(tc.putBackendPoolErr)
			}
			if len(tc.expectedCreateOrUpdateBackendPools) == 2 {
				mockLBClient.EXPECT().CreateOrUpdateBackendPools(
					gomock.Any(),
					gomock.Any(),
					"lb1",
					*tc.expectedCreateOrUpdateBackendPools[1].Name,
					tc.expectedCreateOrUpdateBackendPools[1],
					gomock.Any(),
				).Return(nil)
			}
			cloud.LoadBalancerClient = mockLBClient

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go u.run(ctx)

			//results := sync.Map{}
			for _, op := range tc.operations {
				op := op
				//go func() {
				u.addOperation(op)
				//result := op.wait()
				//results.Store(result, true)
				//}()
				time.Sleep(100 * time.Millisecond)
			}
			time.Sleep(3 * time.Second)
			//err := wait.PollUntilContextTimeout(context.Background(), time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			//	resultLen := 0
			//	results.Range(func(key, value interface{}) bool {
			//		resultLen++
			//		return true
			//	})
			//	return resultLen == tc.expectedResultCount, nil
			//})
			//assert.NoError(t, err)

			//actualResults := make(map[batchOperationResult]bool)
			//poolNameToErrMsg := make(map[string]string)
			//results.Range(func(key, value interface{}) bool {
			//	actualResults[key.(batchOperationResult)] = true
			//	if tc.getBackendPoolErr != nil && !tc.getBackendPoolErr.Retriable ||
			//		tc.putBackendPoolErr != nil && !tc.putBackendPoolErr.Retriable {
			//		poolNameToErrMsg[key.(batchOperationResult).name] = key.(batchOperationResult).err.Error()
			//	}
			//	return true
			//})

			//err = wait.PollUntilContextTimeout(ctx, time.Second, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			//	if tc.expectedResult != nil {
			//		return assert.Equal(t, tc.expectedResult, actualResults, tc.name), nil
			//	}
			//	if tc.expectedPoolNameToErrMsg != nil {
			//		return assert.Equal(t, tc.expectedPoolNameToErrMsg, poolNameToErrMsg, tc.name), nil
			//	}
			//	return false, errors.New("unexpected result")
			//})
			//assert.NoError(t, err)
		})
	}
}

func getTestBackendAddressPoolWithIPs(lbName, bpName string, ips []string) network.BackendAddressPool {
	bp := network.BackendAddressPool{
		ID:   pointer.String(fmt.Sprintf("/subscriptions/subscriptionID/resourceGroups/rg/providers/Microsoft.Network/loadBalancers/%s/backendAddressPools/%s", lbName, bpName)),
		Name: pointer.String(bpName),
		BackendAddressPoolPropertiesFormat: &network.BackendAddressPoolPropertiesFormat{
			Location:                     pointer.String("eastus"),
			LoadBalancerBackendAddresses: &[]network.LoadBalancerBackendAddress{},
		},
	}
	for _, ip := range ips {
		if len(ip) > 0 {
			*bp.LoadBalancerBackendAddresses = append(*bp.LoadBalancerBackendAddresses, network.LoadBalancerBackendAddress{
				Name: pointer.String(""),
				LoadBalancerBackendAddressPropertiesFormat: &network.LoadBalancerBackendAddressPropertiesFormat{
					IPAddress: pointer.String(ip),
				},
			})
		}
	}
	return bp
}

func getTestEndpointSlice(name, namespace, svcName string, nodeNames ...string) *discovery_v1.EndpointSlice {
	endpoints := make([]discovery_v1.Endpoint, 0)
	for _, nodeName := range nodeNames {
		nodeName := nodeName
		endpoints = append(endpoints, discovery_v1.Endpoint{
			NodeName: &nodeName,
		})
	}
	return &discovery_v1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				consts.ServiceNameLabel: svcName,
			},
		},
		Endpoints: endpoints,
	}
}

func TestEndpointSlicesInformer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for _, tc := range []struct {
		name                        string
		existingEPS                 *discovery_v1.EndpointSlice
		updatedEPS                  *discovery_v1.EndpointSlice
		notLocal                    bool
		expectedGetBackendPoolCount int
		expectedPutBackendPoolCount int
	}{
		{
			name:                        "remove unwanted ips and add wanted ones",
			existingEPS:                 getTestEndpointSlice("eps1", "test", "svc1", "node1"),
			updatedEPS:                  getTestEndpointSlice("eps1", "test", "svc1", "node2"),
			expectedGetBackendPoolCount: 1,
			expectedPutBackendPoolCount: 1,
		},
		{
			name:        "skip non-local services",
			existingEPS: getTestEndpointSlice("eps1", "test", "svc2", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "svc2", "node2"),
		},
		{
			name:        "skip an endpoint slice that don't belong to a service",
			existingEPS: getTestEndpointSlice("eps1", "test", "", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "", "node2"),
		},
		{
			name:        "not a local service",
			existingEPS: getTestEndpointSlice("eps1", "test", "", "node1"),
			updatedEPS:  getTestEndpointSlice("eps1", "test", "", "node2"),
			notLocal:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cloud := GetTestCloud(ctrl)
			cloud.localServiceNameToServiceInfoMap = sync.Map{}
			if !tc.notLocal {
				cloud.localServiceNameToServiceInfoMap.Store("test/svc1", &serviceInfo{lbName: "lb1"})
			}
			svc := getTestService("svc1", v1.ProtocolTCP, nil, false)
			client := fake.NewSimpleClientset(&svc, tc.existingEPS)
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			cloud.serviceLister = informerFactory.Core().V1().Services().Lister()
			cloud.LoadBalancerBackendPoolUpdateIntervalInSeconds = 1
			cloud.LoadBalancerSku = consts.LoadBalancerSkuStandard
			cloud.MultipleStandardLoadBalancerConfigurations = []MultipleStandardLoadBalancerConfiguration{
				{
					Name: "lb1",
				},
			}
			cloud.localServiceNameToServiceInfoMap.Store("test/svc1", newServiceInfo(consts.IPVersionIPv4String, "lb1"))
			cloud.nodePrivateIPs = map[string]sets.Set[string]{
				"node1": sets.New[string]("10.0.0.1"),
				"node2": sets.New[string]("10.0.0.2"),
			}

			existingBackendPool := getTestBackendAddressPoolWithIPs("lb1", "test-svc1", []string{"10.0.0.1"})
			expectedBackendPool := getTestBackendAddressPoolWithIPs("lb1", "test-svc1", []string{"10.0.0.2"})
			mockLBClient := mockloadbalancerclient.NewMockInterface(ctrl)
			mockLBClient.EXPECT().GetLBBackendPool(gomock.Any(), gomock.Any(), "lb1", "test-svc1", "").Return(existingBackendPool, nil).Times(tc.expectedGetBackendPoolCount)
			mockLBClient.EXPECT().CreateOrUpdateBackendPools(gomock.Any(), gomock.Any(), "lb1", "test-svc1", expectedBackendPool, "").Return(nil).Times(tc.expectedPutBackendPoolCount)
			cloud.LoadBalancerClient = mockLBClient

			u := newLoadBalancerBackendPoolUpdater(cloud, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cloud.backendPoolUpdater = u
			go cloud.backendPoolUpdater.run(ctx)

			cloud.setUpEndpointSlicesInformer(informerFactory)
			stopChan := make(chan struct{})
			defer func() {
				stopChan <- struct{}{}
			}()
			informerFactory.Start(stopChan)
			time.Sleep(100 * time.Millisecond)

			_, err := client.DiscoveryV1().EndpointSlices("test").Update(context.Background(), tc.updatedEPS, metav1.UpdateOptions{})
			assert.NoError(t, err)
			time.Sleep(2 * time.Second)
		})
	}
}

func TestGetBackendPoolNamesAndIDsForService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	cloud := GetTestCloud(ctrl)
	cloud.MultipleStandardLoadBalancerConfigurations = []MultipleStandardLoadBalancerConfiguration{
		{},
	}
	svc := getTestService("test", v1.ProtocolTCP, nil, false)
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyLocal
	_ = cloud.getBackendPoolNamesForService(&svc, "test")
	_ = cloud.getBackendPoolIDsForService(&svc, "test", "lb")
}
