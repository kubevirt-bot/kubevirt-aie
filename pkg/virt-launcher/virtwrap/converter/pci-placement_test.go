package converter

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"kubevirt.io/kubevirt/pkg/util/hardware"
	"kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/api"
	iommupci "kubevirt.io/kubevirt/pkg/virt-launcher/virtwrap/iommu-pci"
)

type devicePlacementTestCase struct {
	name                  string
	numaCells             []api.NUMACell
	vcpuPins              []api.CPUTuneVCPUPin
	devices               []api.HostDevice
	expectedControllers   int
	expectedExpanderBuses int
	expectedRootPorts     int
	expectedError         string
}

type addDevicesTestCase struct {
	name            string
	devices         []api.HostDevice
	numaCells       []api.NUMACell
	vcpuPins        []api.CPUTuneVCPUPin
	expectedDevices int
	description     string
}

var _ = Describe("PCIe Expander Bus Assigner", func() {
	var (
		originalPciBasePath  string
		originalNodeBasePath string
		fakePciBasePath      string
		fakeNodeBasePath     string
	)

	createDomainSpecWithNUMA := func(numaCells []api.NUMACell, vcpuPins []api.CPUTuneVCPUPin) *api.DomainSpec {
		spec := &api.DomainSpec{
			Devices: api.Devices{
				Controllers: []api.Controller{},
			},
		}
		if len(numaCells) > 0 {
			spec.CPU = api.CPU{
				NUMA: &api.NUMA{Cells: numaCells},
			}
		}
		if len(vcpuPins) > 0 {
			spec.CPUTune = &api.CPUTune{VCPUPin: vcpuPins}
		}
		return spec
	}

	createPCIDevice := func(alias, bus string) api.HostDevice {
		return api.HostDevice{
			Type:  api.HostDevicePCI,
			Alias: api.NewUserDefinedAlias(alias),
			Source: api.HostDeviceSource{
				Address: &api.Address{
					Domain: "0x0000", Bus: bus,
					Slot: "0x00", Function: "0x0",
				},
			},
		}
	}

	createIOMMUPCIDevice := func(alias, bus string) api.HostDevice {
		dev := createPCIDevice(alias, bus)
		dev.ACPI = &api.ACPIHostDev{NodeSet: "tofill"}
		return dev
	}

	createNonPCIDevice := func(deviceType string) api.HostDevice {
		return api.HostDevice{
			Type: deviceType,
		}
	}

	createPCIDeviceWithoutAddress := func(alias string) api.HostDevice {
		return api.HostDevice{
			Type:  api.HostDevicePCI,
			Alias: api.NewUserDefinedAlias(alias),
		}
	}

	setupFakeSysfs := func() {
		var err error
		fakePciBasePath, err = os.MkdirTemp("", "pci_devices")
		Expect(err).ToNot(HaveOccurred())

		fakeNodeBasePath, err = os.MkdirTemp("", "numa_nodes")
		Expect(err).ToNot(HaveOccurred())

		// Create test PCI devices with NUMA nodes.
		// Includes simplified devices (0000:0x:00.0) for basic tests and
		// real GB200 BDFs for hardware-accurate topology tests.
		//
		// Real GB200 layout (from VOYAGER-707 lspci/lscpu):
		//   0008:01:00.0 GPU #1 -> NUMA 0 (Grace CPU #1)
		//   0009:01:00.0 GPU #2 -> NUMA 0 (Grace CPU #1)
		//   0018:01:00.0 GPU #3 -> NUMA 1 (Grace CPU #2)
		//   0019:01:00.0 GPU #4 -> NUMA 1 (Grace CPU #2)
		//   0000:03:00.0 ConnectX-7 IB -> NUMA 0
		//   0010:03:00.0 ConnectX-7 IB -> NUMA 1
		testDevices := map[string]string{
			"0000:01:00.0": "0",
			"0000:02:00.0": "1",
			"0000:03:00.0": "0",
			"0000:04:00.0": "1",
			"0000:05:00.0": "0",
			"0008:01:00.0": "0",
			"0009:01:00.0": "0",
			"0018:01:00.0": "1",
			"0019:01:00.0": "1",
			"0010:03:00.0": "1",
		}

		for pciAddr, numaNode := range testDevices {
			pciDevicePath := filepath.Join(fakePciBasePath, pciAddr)
			err = os.MkdirAll(pciDevicePath, 0o755)
			Expect(err).ToNot(HaveOccurred())

			numaNodeFile := filepath.Join(pciDevicePath, "numa_node")
			err = os.WriteFile(numaNodeFile, []byte(numaNode+"\n"), 0o644)
			Expect(err).ToNot(HaveOccurred())
		}

		// Create NUMA node directories
		for numaID, cpuList := range map[string]string{"0": "0-3", "1": "4-7"} {
			numaNodePath := filepath.Join(fakeNodeBasePath, "node"+numaID)
			err = os.MkdirAll(numaNodePath, 0o755)
			Expect(err).ToNot(HaveOccurred())

			cpuListFile := filepath.Join(numaNodePath, "cpulist")
			err = os.WriteFile(cpuListFile, []byte(cpuList+"\n"), 0o644)
			Expect(err).ToNot(HaveOccurred())
		}
	}

	BeforeEach(func() {
		originalPciBasePath = hardware.PciBasePath
		originalNodeBasePath = hardware.NodeBasePath
		setupFakeSysfs()
		hardware.PciBasePath = fakePciBasePath
		hardware.NodeBasePath = fakeNodeBasePath
		iommupci.ParseConfigHybridFn = func(_ string) (bool, bool, bool, int, int, error) {
			return false, false, false, 0, 48, nil
		}
		iommupci.CalculatePCIHole64SizeFn = func(_ string) (uint64, error) {
			return 0, nil
		}
	})

	AfterEach(func() {
		hardware.PciBasePath = originalPciBasePath
		hardware.NodeBasePath = originalNodeBasePath
		iommupci.ParseConfigHybridFn = nil
		iommupci.CalculatePCIHole64SizeFn = nil
		if fakePciBasePath != "" {
			os.RemoveAll(fakePciBasePath)
		}
		if fakeNodeBasePath != "" {
			os.RemoveAll(fakeNodeBasePath)
		}
	})

	Describe("getCurrentControllerIndex", func() {
		It("should return the highest index of the existing controllers", func() {
			domainSpec := &api.DomainSpec{
				Devices: api.Devices{
					Controllers: []api.Controller{
						{Model: api.ControllerModelPCIeRoot, Index: "0"},
						{Model: api.ControllerModelPCIeRootPort, Index: "4"},
					},
				},
			}

			Expect(getCurrentControllerIndex(domainSpec)).To(Equal(uint32(4)))
		})
	})

	Describe("expanderBusAssigner", func() {
		var (
			assigner   *expanderBusAssigner
			domainSpec *api.DomainSpec
			iommuPCI   *iommupci.IommuPCI
		)

		BeforeEach(func() {
			domainSpec = createDomainSpecWithNUMA(
				[]api.NUMACell{{ID: "0", CPUs: "0-1"}, {ID: "1", CPUs: "2-3"}},
				[]api.CPUTuneVCPUPin{{VCPU: 0, CPUSet: "0"}, {VCPU: 2, CPUSet: "4"}},
			)
			iommuPCI = iommupci.NewIommuPCI(runtime.GOARCH)
			assigner = newExpanderBusAssigner(domainSpec, iommuPCI)
		})

		DescribeTable("addDevices",
			func(testCase addDevicesTestCase) {
				if testCase.numaCells != nil || testCase.vcpuPins != nil {
					domainSpec = createDomainSpecWithNUMA(testCase.numaCells, testCase.vcpuPins)
					domainSpec.Devices.HostDevices = testCase.devices
					assigner = newExpanderBusAssigner(domainSpec, iommuPCI)
				}

				assigner.addDevices(testCase.devices)
				Expect(assigner.devices).To(HaveLen(testCase.expectedDevices), testCase.description)
			},
			Entry("filters non-PCI devices", addDevicesTestCase{
				name: "mixed device types",
				devices: []api.HostDevice{
					createPCIDevice("pci1", "0x01"),
					createNonPCIDevice("usb"),
					createPCIDevice("pci2", "0x02"),
					createNonPCIDevice("scsi"),
				},
				expectedDevices: 2,
				description:     "should only accept PCI devices",
			}),
			Entry("filters devices without source address", addDevicesTestCase{
				name: "devices without address",
				devices: []api.HostDevice{
					createPCIDevice("pci1", "0x01"),
					createPCIDeviceWithoutAddress("pci2"),
				},
				expectedDevices: 1,
				description:     "should skip devices without source address",
			}),
			Entry("filters devices without NUMA affinity", addDevicesTestCase{
				name:            "devices without NUMA topology",
				devices:         []api.HostDevice{createPCIDevice("pci1", "0x01")},
				numaCells:       []api.NUMACell{},
				vcpuPins:        []api.CPUTuneVCPUPin{},
				expectedDevices: 0,
				description:     "should skip devices when no NUMA topology is configured",
			}),
			Entry("accepts PCI devices with NUMA affinity", addDevicesTestCase{
				name: "valid PCI devices",
				devices: []api.HostDevice{
					createPCIDevice("device1", "0x01"),
					createPCIDevice("device2", "0x02"),
				},
				expectedDevices: 2,
				description:     "should accept all valid PCI devices with NUMA affinity",
			}),
		)

		DescribeTable("PlaceNumaAlignedDevices",
			func(testCase devicePlacementTestCase) {
				if testCase.numaCells != nil || testCase.vcpuPins != nil {
					domainSpec = createDomainSpecWithNUMA(testCase.numaCells, testCase.vcpuPins)
					assigner = newExpanderBusAssigner(domainSpec, iommuPCI)
				}

				domainSpec.Devices.HostDevices = testCase.devices

				err := assigner.PlaceNumaAlignedDevices()

				if testCase.expectedError != "" {
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring(testCase.expectedError))
				} else {
					Expect(err).ToNot(HaveOccurred())
				}

				if testCase.expectedControllers >= 0 {
					Expect(domainSpec.Devices.Controllers).To(HaveLen(testCase.expectedControllers))
				}

				if testCase.expectedExpanderBuses > 0 {
					expanderBusCount := 0
					for _, controller := range domainSpec.Devices.Controllers {
						if controller.Model == api.ControllerModelPCIeExpanderBus {
							expanderBusCount++
						}
					}
					Expect(expanderBusCount).To(Equal(testCase.expectedExpanderBuses))
				}

				if testCase.expectedRootPorts > 0 {
					rootPortCount := 0
					for _, controller := range domainSpec.Devices.Controllers {
						if controller.Model == api.ControllerModelPCIeRootPort {
							rootPortCount++
						}
					}
					Expect(rootPortCount).To(Equal(testCase.expectedRootPorts))
				}
			},
			Entry("handles empty device list", devicePlacementTestCase{
				name:                "no devices",
				devices:             []api.HostDevice{},
				expectedControllers: 0,
			}),
			Entry("places single device on single NUMA node", devicePlacementTestCase{
				name:                  "single device",
				devices:               []api.HostDevice{createPCIDevice("device1", "0x01")},
				expectedControllers:   2,
				expectedExpanderBuses: 1,
				expectedRootPorts:     1,
			}),
			Entry("places multiple devices on same NUMA node", devicePlacementTestCase{
				name: "multiple devices same NUMA",
				devices: []api.HostDevice{
					createPCIDevice("device1", "0x01"),
					createPCIDevice("device2", "0x03"),
				},
				expectedControllers:   3,
				expectedExpanderBuses: 1,
				expectedRootPorts:     2,
			}),
			Entry("places devices on different NUMA nodes", devicePlacementTestCase{
				name: "devices on different NUMA nodes",
				devices: []api.HostDevice{
					createPCIDevice("device_numa0", "0x01"),
					createPCIDevice("device_numa1", "0x02"),
				},
				expectedControllers:   4,
				expectedExpanderBuses: 2,
				expectedRootPorts:     2,
			}),
			Entry("handles domain spec without NUMA topology", devicePlacementTestCase{
				name:                "no NUMA topology",
				numaCells:           []api.NUMACell{},
				vcpuPins:            []api.CPUTuneVCPUPin{},
				devices:             []api.HostDevice{createPCIDevice("device1", "0x01")},
				expectedControllers: 0,
			}),
			Entry("handles domain spec without CPU affinity", devicePlacementTestCase{
				name:                "no CPU affinity",
				numaCells:           []api.NUMACell{{ID: "0", CPUs: "0-1"}},
				vcpuPins:            []api.CPUTuneVCPUPin{},
				devices:             []api.HostDevice{createPCIDevice("device1", "0x01")},
				expectedControllers: 0,
			}),
			Entry("places single IOMMU device with dedicated expander bus", devicePlacementTestCase{
				name:                  "single IOMMU device",
				devices:               []api.HostDevice{createIOMMUPCIDevice("gpu1", "0x01")},
				expectedControllers:   2,
				expectedExpanderBuses: 1,
				expectedRootPorts:     1,
			}),
			Entry("places multiple IOMMU devices on same NUMA node with separate expander buses", devicePlacementTestCase{
				name: "multiple IOMMU devices same NUMA",
				devices: []api.HostDevice{
					createIOMMUPCIDevice("gpu1", "0x01"),
					createIOMMUPCIDevice("gpu2", "0x03"),
				},
				expectedControllers:   4,
				expectedExpanderBuses: 2,
				expectedRootPorts:     2,
			}),
			Entry("places IOMMU devices on different NUMA nodes with separate expander buses", devicePlacementTestCase{
				name: "IOMMU devices on different NUMA nodes",
				devices: []api.HostDevice{
					createIOMMUPCIDevice("gpu_numa0", "0x01"),
					createIOMMUPCIDevice("gpu_numa1", "0x02"),
				},
				expectedControllers:   4,
				expectedExpanderBuses: 2,
				expectedRootPorts:     2,
			}),
		)
	})

	Describe("IOMMU device topology isolation", func() {
		var (
			domainSpec *api.DomainSpec
			iommuPCI   *iommupci.IommuPCI
		)

		BeforeEach(func() {
			domainSpec = createDomainSpecWithNUMA(
				[]api.NUMACell{{ID: "0", CPUs: "0-1"}, {ID: "1", CPUs: "2-3"}},
				[]api.CPUTuneVCPUPin{{VCPU: 0, CPUSet: "0"}, {VCPU: 2, CPUSet: "4"}},
			)
			iommuPCI = iommupci.NewIommuPCI(runtime.GOARCH)
		})

		It("should create separate smmuv3 IOMMU devices for each GPU on the same NUMA node", func() {
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x01"),
				createIOMMUPCIDevice("gpu2", "0x03"),
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			Expect(domainSpec.Devices.IOMMU).To(HaveLen(2), "each GPU should have its own smmuv3")
			Expect(domainSpec.Devices.IOMMU[0].Model).To(Equal("smmuv3"))
			Expect(domainSpec.Devices.IOMMU[1].Model).To(Equal("smmuv3"))

			Expect(domainSpec.Devices.IOMMU[0].Driver.PciBus).ToNot(Equal(domainSpec.Devices.IOMMU[1].Driver.PciBus),
				"each smmuv3 should reference a different expander bus")
		})

		It("should assign non-overlapping bus numbers to per-device expander buses", func() {
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x01"),
				createIOMMUPCIDevice("gpu2", "0x03"),
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			busNumbers := map[uint32]bool{}
			for _, controller := range domainSpec.Devices.Controllers {
				if controller.Model == api.ControllerModelPCIeExpanderBus {
					Expect(controller.Target).ToNot(BeNil())
					Expect(controller.Target.BusNr).ToNot(BeNil())
					busNr := *controller.Target.BusNr
					Expect(busNumbers[busNr]).To(BeFalse(), "bus number %d assigned twice", busNr)
					busNumbers[busNr] = true
				}
			}
			Expect(busNumbers).To(HaveLen(2))
		})

		It("should use shared per-NUMA expander bus when only one IOMMU device on a NUMA node", func() {
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x01"),
				createPCIDevice("nic1", "0x03"),
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			expanderBuses := 0
			for _, controller := range domainSpec.Devices.Controllers {
				if controller.Model == api.ControllerModelPCIeExpanderBus {
					expanderBuses++
				}
			}
			Expect(expanderBuses).To(Equal(1), "single IOMMU device should share the per-NUMA expander bus")

			Expect(domainSpec.Devices.IOMMU).To(HaveLen(1), "single IOMMU device should still get an smmuv3")
			Expect(domainSpec.Devices.IOMMU[0].Model).To(Equal("smmuv3"))
		})

		It("should assign each GPU device to its own root port on separate expander buses", func() {
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x01"),
				createIOMMUPCIDevice("gpu2", "0x03"),
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			deviceBuses := map[string]bool{}
			for _, device := range domainSpec.Devices.HostDevices {
				Expect(device.Address).ToNot(BeNil())
				Expect(device.Address.Bus).ToNot(BeEmpty())
				Expect(deviceBuses[device.Address.Bus]).To(BeFalse(),
					"bus %s used by multiple devices", device.Address.Bus)
				deviceBuses[device.Address.Bus] = true
			}
			Expect(deviceBuses).To(HaveLen(2))
		})

		It("should isolate all 4 GPUs across 2 sockets on a GB200", func() {
			// Real GB200 2-superchip topology: 2 GPUs per CPU socket
			//   GPU 1 (0008:01:00.0): NUMA 0
			//   GPU 2 (0009:01:00.0): NUMA 0
			//   GPU 3 (0018:01:00.0): NUMA 1
			//   GPU 4 (0019:01:00.0): NUMA 1
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x08:01"),
				createIOMMUPCIDevice("gpu2", "0x09:01"),
				createIOMMUPCIDevice("gpu3", "0x18:01"),
				createIOMMUPCIDevice("gpu4", "0x19:01"),
			}
			// Fix source addresses to use real GB200 BDFs
			for i, domain := range []string{"0x0008", "0x0009", "0x0018", "0x0019"} {
				domainSpec.Devices.HostDevices[i].Source.Address = &api.Address{
					Domain: domain, Bus: "0x01", Slot: "0x00", Function: "0x0",
				}
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			// 4 dedicated expander buses (one per GPU)
			expanderBuses := 0
			numaNodes := map[uint32]int{}
			for _, controller := range domainSpec.Devices.Controllers {
				if controller.Model == api.ControllerModelPCIeExpanderBus {
					expanderBuses++
					Expect(controller.Target).ToNot(BeNil())
					Expect(controller.Target.NUMANode).ToNot(BeNil())
					numaNodes[*controller.Target.NUMANode]++
				}
			}
			Expect(expanderBuses).To(Equal(4), "each GPU should have its own expander bus")
			Expect(numaNodes[0]).To(Equal(2), "2 expander buses on NUMA 0")
			Expect(numaNodes[1]).To(Equal(2), "2 expander buses on NUMA 1")

			// 4 IOMMU devices (one per GPU)
			Expect(domainSpec.Devices.IOMMU).To(HaveLen(4))

			// All 4 GPUs on different buses
			deviceBuses := map[string]bool{}
			for _, device := range domainSpec.Devices.HostDevices {
				Expect(device.Address).ToNot(BeNil())
				Expect(deviceBuses[device.Address.Bus]).To(BeFalse(),
					"bus %s used by multiple devices", device.Address.Bus)
				deviceBuses[device.Address.Bus] = true
			}
			Expect(deviceBuses).To(HaveLen(4))
		})

		It("should isolate GPUs while NICs share the per-NUMA bus on a GB200", func() {
			// 2 GPUs + 1 IB NIC on NUMA 0: GPUs get isolated, NIC shares per-NUMA bus
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x08:01"),
				createIOMMUPCIDevice("gpu2", "0x09:01"),
				createPCIDevice("ib0", "0x03"),
			}
			for i, domain := range []string{"0x0008", "0x0009"} {
				domainSpec.Devices.HostDevices[i].Source.Address = &api.Address{
					Domain: domain, Bus: "0x01", Slot: "0x00", Function: "0x0",
				}
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			// 3 expander buses: 2 dedicated (GPUs) + 1 shared (NIC)
			expanderBuses := 0
			for _, controller := range domainSpec.Devices.Controllers {
				if controller.Model == api.ControllerModelPCIeExpanderBus {
					expanderBuses++
				}
			}
			Expect(expanderBuses).To(Equal(3))

			// 2 IOMMU devices (GPUs only)
			Expect(domainSpec.Devices.IOMMU).To(HaveLen(2))

			// NIC should be on a different bus than both GPUs
			gpuBuses := map[string]bool{}
			for _, device := range domainSpec.Devices.HostDevices[:2] {
				gpuBuses[device.Address.Bus] = true
			}
			nicBus := domainSpec.Devices.HostDevices[2].Address.Bus
			Expect(gpuBuses).ToNot(HaveKey(nicBus), "NIC should not share a bus with GPUs")
		})

		It("should use shared per-NUMA bus when passing through 1 GPU per socket", func() {
			// Partial passthrough: 1 GPU from each socket
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createIOMMUPCIDevice("gpu1", "0x08:01"),
				createIOMMUPCIDevice("gpu3", "0x18:01"),
			}
			domainSpec.Devices.HostDevices[0].Source.Address = &api.Address{
				Domain: "0x0008", Bus: "0x01", Slot: "0x00", Function: "0x0",
			}
			domainSpec.Devices.HostDevices[1].Source.Address = &api.Address{
				Domain: "0x0018", Bus: "0x01", Slot: "0x00", Function: "0x0",
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			// 2 shared expander buses (one per NUMA, no isolation needed)
			expanderBuses := 0
			for _, controller := range domainSpec.Devices.Controllers {
				if controller.Model == api.ControllerModelPCIeExpanderBus {
					expanderBuses++
				}
			}
			Expect(expanderBuses).To(Equal(2), "single GPU per NUMA should use shared bus")

			// 2 IOMMU devices (one per GPU, each on its own NUMA's shared bus)
			Expect(domainSpec.Devices.IOMMU).To(HaveLen(2))
		})
	})

	Describe("PlacePCIDevicesWithNUMAAlignment", func() {
		var (
			domainSpec *api.DomainSpec
			iommuPCI   *iommupci.IommuPCI
		)

		BeforeEach(func() {
			domainSpec = createDomainSpecWithNUMA(
				[]api.NUMACell{{ID: "0", CPUs: "0-1"}, {ID: "1", CPUs: "2-3"}},
				[]api.CPUTuneVCPUPin{{VCPU: 0, CPUSet: "0"}, {VCPU: 2, CPUSet: "4"}},
			)
			iommuPCI = iommupci.NewIommuPCI(runtime.GOARCH)
		})

		It("should return error when controller index exceeds the last expander bus number", func() {
			// Set current controller index to the maximum to trigger the validation
			domainSpec.Devices.Controllers = []api.Controller{
				{Model: api.ControllerModelPCIeRootPort, Index: strconv.Itoa(maxExpanderBusNr)},
			}

			// Add a device, this would require creating new controllers
			domainSpec.Devices.HostDevices = []api.HostDevice{createPCIDevice("device1", "0x01")}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)

			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("insufficient bus numbers for NUMA-aligned PCIe topology"))
			Expect(err.Error()).To(ContainSubstring("current controller index 256"))
			Expect(err.Error()).To(ContainSubstring("last assigned expander bus number 255"))
		})

		It("should assign bus numbers for expander buses calculated as maxBusNr - controllerCount + 1", func() {
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createPCIDevice("device1", "0x01"),
				createPCIDevice("device2", "0x02"),
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			// Bus numbers calculated as 254 - controllerCount + 1:
			// NUMA 0: 255 - 2 + 1 = 254 (after creating expander bus + root port)
			// NUMA 1: 255 - 4 + 1 = 252 (after creating 2nd expander bus + root port)
			expectedBusNumbers := map[uint32]bool{254: false, 252: false}
			for _, controller := range domainSpec.Devices.Controllers {
				if controller.Model == api.ControllerModelPCIeExpanderBus {
					Expect(controller.Target).ToNot(BeNil())
					Expect(controller.Target.BusNr).ToNot(BeNil())
					busNr := *controller.Target.BusNr
					_, expected := expectedBusNumbers[busNr]
					Expect(expected).To(BeTrue(), "Bus number %d should be one of the expected values (253, 251)", busNr)
					expectedBusNumbers[busNr] = true
				}
			}

			// Ensure both expected bus numbers were assigned
			for busNr, assigned := range expectedBusNumbers {
				Expect(assigned).To(BeTrue(), "Expected bus number %d was not assigned", busNr)
			}
		})

		It("should assign devices to correct root ports", func() {
			domainSpec.Devices.HostDevices = []api.HostDevice{
				createPCIDevice("device1", "0x01"),
				createPCIDevice("device2", "0x02"),
			}

			err := PlacePCIDevicesWithNUMAAlignment(domainSpec, iommuPCI)
			Expect(err).ToNot(HaveOccurred())

			for _, device := range domainSpec.Devices.HostDevices {
				Expect(device.Address).ToNot(BeNil())
				Expect(device.Address.Type).To(Equal(api.AddressPCI))
				Expect(device.Address.Bus).ToNot(BeEmpty())
				Expect(device.Address.Slot).To(Equal("0x00"))
			}
		})
	})
})
