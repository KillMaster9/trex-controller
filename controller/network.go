package main

import (
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"net"
	"os"
	"path/filepath"
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
	id := fmt.Sprintf("%s_%s", name, pauseID[:3])
	return fmt.Sprintf("trex_%s", id), fmt.Sprintf("tmp%s", id)
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
		portIndex := string(port.VFIndex)
		logger.Println(fmt.Sprintf("Configure VF %s Network", portIndex))
		vfName := fmt.Sprintf("%sv%s", parentIfName, portIndex)
		vfPciAddress, err := getVFPciAddress(parentIfName, vfName)
		if err != nil {
			return nil, err
		}
		vfPCIMap[vfName] = vfPciAddress

		if err = setVFVlan(parentIfName, vfName, port.VlanId); err != nil && err != syscall.EEXIST {
			return nil, err
		}
	}

	return vfPCIMap, nil
}

// getVFPciAddress 通过父接口名和VF名获取VF的PCI地址
func getVFPciAddress(parentIfName, vfName string) (string, error) {
	// 获取VF网络接口
	vfLink, err := netlink.LinkByName(vfName)
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
	vfIndex := vfLink.Attrs().Index

	// 通过sysfs查找VF的PCI地址
	pciAddress, err := findVFPciAddress(parentIfName, vfIndex)
	if err != nil {
		return "", fmt.Errorf("failed to find VF PCI address: %v", err)
	}

	logger.Println(fmt.Sprintf("VF %s PCI Address: %s", vfName, pciAddress))

	return pciAddress, nil
}

// findVFPciAddress 通过sysfs查找VF的PCI地址
func findVFPciAddress(parentIfName string, vfIndex int) (string, error) {
	// 构建sysfs路径
	sysfsPath := fmt.Sprintf("/sys/class/net/%s/device/virtfn%d", parentIfName, vfIndex)

	// 解析PCI地址的符号链接
	pciPath, err := filepath.EvalSymlinks(sysfsPath)
	if err != nil {
		return "", err
	}

	// 从路径中提取PCI地址
	pciAddress := filepath.Base(pciPath)
	if !strings.HasPrefix(pciAddress, "0000:") {
		pciAddress = "0000:" + pciAddress
	}

	return pciAddress, nil
}

// setVFVlan 设置VF的VLAN ID
func setVFVlan(parentIfName, vfName string, vlanID int) error {
	// 获取父接口
	parentLink, err := netlink.LinkByName(parentIfName)
	if err != nil {
		return fmt.Errorf("failed to get parent link: %v", err)
	}

	// 获取VF网络接口
	vfLink, err := netlink.LinkByName(vfName)
	if err != nil {
		return fmt.Errorf("failed to get VF link: %v", err)
	}

	// 获取VF索引
	vfIndex := vfLink.Attrs().Index

	// 设置VF的VLAN
	if err := netlink.LinkSetVfVlan(parentLink, vfIndex, vlanID); err != nil {
		return fmt.Errorf("failed to set VF VLAN: %v", err)
	}

	return nil
}
