package main

import (
	"bufio"
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func bridgeByName(name string) (*netlink.Bridge, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("could not lookup %q: %v", name, err)
	}
	br, ok := l.(*netlink.Bridge)
	if !ok {
		return nil, fmt.Errorf("%q already exists but is not a bridge", name)
	}
	return br, nil
}

func EnsureBridge(brName string, mtu int, promiscMode, vlanFiltering bool) (*netlink.Bridge, error) {
	// Create trex bridge, Name: trex-br
	br := &netlink.Bridge{
		LinkAttrs: netlink.LinkAttrs{
			Name: brName,
			MTU:  mtu,
			// Let kernel use default txqueuelen; leaving it unset
			// means 0, and a zero-length TX queue messes up FIFO
			// traffic shapers which use TX queue length as the
			// default packet limit
			TxQLen: -1,
		},
	}
	if vlanFiltering {
		br.VlanFiltering = &vlanFiltering
	}

	err := netlink.LinkAdd(br)
	if err != nil && err != syscall.EEXIST {
		return nil, fmt.Errorf("could not add %q: %v", brName, err)
	}

	//if promiscMode {
	//	if err := netlink.SetPromiscOn(br); err != nil {
	//		return nil, fmt.Errorf("could not set promiscuous mode on %q: %v", brName, err)
	//	}
	//}

	// Re-fetch link to read all attributes and if it already existed,
	// ensure it's really a bridge with similar configuration
	br, err = bridgeByName(brName)
	if err != nil {
		return nil, err
	}

	if err := netlink.LinkSetUp(br); err != nil {
		return nil, err
	}

	logger.Println(fmt.Sprintf("Created bridge %s Successed!", brName))

	return br, nil
}

func getPairName(name, pauseID string) (string, string) {
	if len(name) > 10 {
		name = name[:9]
	}
	return fmt.Sprintf("trex_%s", name), fmt.Sprintf("tmp%s", name)
}

func configurePauseContainerNetwork(config TRExConfig, pid int, br *netlink.Bridge, pauseID string) (map[string]string, error) {
	// 使用网络命名空间文件路径
	vethHost, vethCont := getPairName(config.Metadata.Name, pauseID)

	// 创建veth pair
	hostVeth, contVeth, err := createVethPair(vethHost, vethCont, 1500)
	if err != nil {
		return nil, err
	}

	// 将host端veth连接到网桥
	if err := netlink.LinkSetMaster(hostVeth, br); err != nil {
		return nil, fmt.Errorf("failed to connect veth to bridge: %v", err)
	}

	// 启用host端veth
	if err := netlink.LinkSetUp(hostVeth); err != nil {
		return nil, fmt.Errorf("failed to set host veth up: %v", err)
	}
	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	if err := netlink.LinkSetNsFd(contVeth, int(netnsPathFD(netnsPath))); err != nil {
		return nil, fmt.Errorf("failed to move veth to container: %v", err)
	}

	vfPCIMap := make(map[string]string)

	// 配置VF vlanID
	if config.Spec.NetworkType == "SRIOV" {
		vfPCIMap, err = configVFNetwork(config)
		if err != nil {
			return nil, err
		}
	}

	// 进入网络命名空间配置
	return vfPCIMap, ns.WithNetNSPath(netnsPath, func(_ ns.NetNS) error {
		// 重命名容器端veth
		if err := netlink.LinkSetName(contVeth, "mgmt"); err != nil {
			return fmt.Errorf("failed to rename container veth: %v", err)
		}
		eth0, err := netlink.LinkByName("mgmt")
		if err != nil {
			return fmt.Errorf("failed to find mgmt: %v", err)
		}

		// 启用容器端接口
		if err := netlink.LinkSetUp(eth0); err != nil {
			return fmt.Errorf("failed to set mgmt up: %v", err)
		}

		// 添加IP地址
		if !strings.Contains(config.Spec.MgmtIP, "/") {
			config.Spec.MgmtIP = fmt.Sprintf("%s/32", config.Spec.MgmtIP)
		}
		addr, err := netlink.ParseAddr(config.Spec.MgmtIP)
		if err != nil {
			return fmt.Errorf("failed to parse IP address: %v", err)
		}
		if err := netlink.AddrAdd(eth0, addr); err != nil {
			return fmt.Errorf("failed to add IP address: %v", err)
		}

		// 添加默认路由
		route := netlink.Route{
			Dst: nil,
			Gw:  net.ParseIP(config.Spec.MgmtGateway),
		}
		if err := netlink.RouteAdd(&route); err != nil && err != syscall.EEXIST {
			if err == syscall.ENETUNREACH {
				log.Printf("Warning: Network unreachable when adding default route, continuing anyway")
				return nil
			}
			return fmt.Errorf("failed to add default route: %v", err)
		}

		return nil
	})
}

func createVethPair(hostName, contName string, mtu int) (netlink.Link, netlink.Link, error) {
	// 清理可能存在的残留接口
	if link, err := netlink.LinkByName(hostName); err == nil {
		netlink.LinkDel(link)
	}
	if link, err := netlink.LinkByName(contName); err == nil {
		netlink.LinkDel(link)
	}

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: hostName,
			MTU:  mtu,
		},
		PeerName: contName,
	}

	if err := netlink.LinkAdd(veth); err != nil {
		return nil, nil, fmt.Errorf("failed to create veth pair: %v", err)
	}

	hostVeth, err := netlink.LinkByName(hostName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find host veth: %v", err)
	}

	contVeth, err := netlink.LinkByName(contName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find container veth: %v", err)
	}

	return hostVeth, contVeth, nil
}

// 辅助函数：获取网络命名空间文件描述符
func netnsPathFD(netnsPath string) uintptr {
	file, err := os.Open(netnsPath)
	if err != nil {
		panic(fmt.Sprintf("failed to open netns path %s: %v", netnsPath, err))
	}
	return file.Fd()
}

func configVFNetwork(config TRExConfig) (map[string]string, error) {
	parentIfName := config.Spec.ParentInterface
	vfPCIMap := make(map[string]string)

	for _, port := range config.Spec.Port {
		portIndex := strconv.Itoa(port.VFIndex)
		//logger.Println(fmt.Sprintf("Configure VF %s Network", portIndex))
		vfName := fmt.Sprintf("%sv%s", parentIfName, portIndex)
		logger.Println(fmt.Sprintf("Configure VF %s Network", vfName))
		vfPciAddress, err := getVFPciAddress(parentIfName, vfName)
		if err != nil {
			return nil, err
		}
		vfPCIMap[vfName] = vfPciAddress

		if err = setVFVlan(parentIfName, port.VFIndex, port.VlanId); err != nil && err != syscall.EEXIST {
			logger.Println(fmt.Sprintf("Warning: Failed to set VF VLAN ID: %v", err))
			return nil, err
		}
	}

	return vfPCIMap, nil
}

// getVFPciAddress 通过父接口名和VF名获取VF的PCI地址
func getVFPciAddress(parentIfName, vfName string) (string, error) {
	// 获取VF网络接口
	_, err := netlink.LinkByName(vfName)
	if err != nil {
		return "", fmt.Errorf("failed to get VF link: %v", err)
	}

	// 获取父接口
	parentLink, err := netlink.LinkByName(parentIfName)
	if err != nil {
		return "", fmt.Errorf("failed to get parent link: %v", err)
	}

	// 检查父接口是否是SR-IOV支持的设备
	if parentLink.Type() != "device" {
		return "", fmt.Errorf("parent interface is not a physical device")
	}

	// 获取VF的索引
	//vfIndex := vfLink.Attrs().Index

	// 通过sysfs查找VF的PCI地址
	pciAddress, err := findVFPciAddress(parentIfName, vfName)
	if err != nil {
		return "", fmt.Errorf("failed to find VF PCI address: %v", err)
	}

	logger.Println(fmt.Sprintf("VF %s PCI Address: %s", vfName, pciAddress))

	return pciAddress, nil
}

// findVFPciAddress 通过sysfs查找VF的PCI地址
func findVFPciAddress(parentIfName string, vfName string) (string, error) {
	// 构建sysfs路径
	//vfName := fmt.Sprintf("%sv%d", parentIfName, vfIndex)

	ifacePath := filepath.Join("/sys/class/net", vfName)
	if _, err := os.Stat(ifacePath); os.IsNotExist(err) {
		logger.Println(fmt.Sprintf("VF %s not exist", vfName))

		return "", fmt.Errorf("VF %s not exist", vfName)
	}

	// 获取设备符号链接
	devicePath := filepath.Join(ifacePath, "device")
	deviceSymlink, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", fmt.Errorf("unable to resolve device symbolic link: %v", err)
	}

	// 从设备路径中提取PCI地址
	pciAddr := extractPCIAddress(deviceSymlink)
	if pciAddr == "" {
		// 备用方法：尝试从uevent文件读取
		ueventPath := filepath.Join(devicePath, "uevent")
		pciAddr, err = extractPCIFromUevent(ueventPath)
		if err != nil {
			return "", fmt.Errorf("unable to extract PCI address from uevent file: %v", err)
		}
	}

	if pciAddr == "" {
		return "", fmt.Errorf("unable to determine PCI address for network interface %s", vfName)
	}

	return pciAddr, nil
}
func extractPCIAddress(devicePath string) string {
	// 设备路径通常包含PCI地址作为最后一部分
	// 例如: /sys/devices/pci0000:00/0000:00:02.0/0000:01:00.0/net/eth1
	parts := strings.Split(devicePath, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		// PCI地址格式: [domain:]bus:device.function
		if strings.Contains(part, ":") && strings.Contains(part, ".") {
			return part
		}
	}
	return ""
}

// extractPCIFromUevent 从uevent文件中提取PCI地址
func extractPCIFromUevent(ueventPath string) (string, error) {
	file, err := os.Open(ueventPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "PCI_SLOT_NAME=") {
			// 格式: PCI_SLOT_NAME=0000:01:00.0
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				return parts[1], nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("PCI_SLOT_NAME not found in uevent file")
}

// setVFVlan 设置VF的VLAN ID
func setVFVlan(parentIfName string, vfIndex int, vlanID int) error {
	// 获取父接口
	parentLink, err := netlink.LinkByName(parentIfName)
	if err != nil {
		return fmt.Errorf("failed to get parent link: %v", err)
	}

	//// 获取VF网络接口
	//vfLink, err := netlink.LinkByName(vfName)
	//if err != nil {
	//	return fmt.Errorf("failed to get VF link: %v", err)
	//}
	//
	//// 获取VF索引
	//vfIndex := vfLink.Attrs().Index

	// 设置VF的VLAN
	if err := netlink.LinkSetVfVlan(parentLink, vfIndex, vlanID); err != nil {
		return fmt.Errorf("failed to set VF VLAN: %v", err)
	}

	logger.Println(fmt.Sprintf("Set VF %s VLAN ID: %d Success!", fmt.Sprintf("%sv%d", parentIfName, vfIndex), vlanID))

	return nil
}
