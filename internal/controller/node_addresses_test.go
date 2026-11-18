/*
Copyright 2025 SUSE.

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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
)

var _ = Describe("nodeAddressesFromVMI", func() {
	vmi := func(interfaces ...kubevirtv1.VirtualMachineInstanceNetworkInterface) *kubevirtv1.VirtualMachineInstance {
		return &kubevirtv1.VirtualMachineInstance{Status: kubevirtv1.VirtualMachineInstanceStatus{Interfaces: interfaces}}
	}

	internalIP := func(ip string) corev1.NodeAddress {
		return corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip}
	}
	hostname := corev1.NodeAddress{Type: corev1.NodeHostName, Address: "node-1"}

	It("publishes the address of the VM's interface and the host name", func() {
		Expect(nodeAddressesFromVMI(vmi(
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-1", InterfaceName: "eth0", IP: "192.0.2.21", IPs: []string{"192.0.2.21", "fe80::1"}},
		), "node-1", "192.0.2.10")).To(Equal([]corev1.NodeAddress{internalIP("192.0.2.21"), hostname}))
	})

	It("leaves out interfaces that exist only inside the guest (CNI)", func() {
		Expect(nodeAddressesFromVMI(vmi(
			kubevirtv1.VirtualMachineInstanceNetworkInterface{InterfaceName: "flannel.1", IP: "10.42.0.0", IPs: []string{"10.42.0.0"}},
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-1", InterfaceName: "eth0", IP: "192.0.2.21", IPs: []string{"192.0.2.21"}},
			kubevirtv1.VirtualMachineInstanceNetworkInterface{InterfaceName: "cali0123", IP: "fe80::ecee:eeff:feee:eeee"},
		), "node-1", "")).To(Equal([]corev1.NodeAddress{internalIP("192.0.2.21"), hostname}))
	})

	It("leaves out the control plane endpoint held by this node, even when it is reported first", func() {
		Expect(nodeAddressesFromVMI(vmi(
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-1", IP: "192.0.2.10", IPs: []string{"192.0.2.10", "192.0.2.21"}},
		), "node-1", "192.0.2.10")).To(Equal([]corev1.NodeAddress{internalIP("192.0.2.21"), hostname}))
	})

	It("leaves out link-local addresses", func() {
		Expect(nodeAddressesFromVMI(vmi(
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-1", IP: "fe80::1", IPs: []string{"fe80::1", "192.0.2.21"}},
		), "node-1", "")).To(Equal([]corev1.NodeAddress{internalIP("192.0.2.21"), hostname}))
	})

	It("publishes one address per interface of the VM definition", func() {
		Expect(nodeAddressesFromVMI(vmi(
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-1", IP: "192.0.2.21", IPs: []string{"192.0.2.21"}},
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-2", IP: "198.51.100.21"},
		), "node-1", "")).To(Equal([]corev1.NodeAddress{internalIP("192.0.2.21"), internalIP("198.51.100.21"), hostname}))
	})

	It("returns nothing, not a lone host name, while the guest agent reports no address", func() {
		Expect(nodeAddressesFromVMI(vmi(
			kubevirtv1.VirtualMachineInstanceNetworkInterface{Name: "nic-1"},
		), "node-1", "")).To(BeNil())
		Expect(nodeAddressesFromVMI(vmi(), "node-1", "")).To(BeNil())
	})
})
