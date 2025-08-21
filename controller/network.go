package main

import (
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"net"
	"os"
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

func getPairName(pauseID string) (string, string) {
	id := pauseID[:7]
	return fmt.Sprintf("trex%s", id), fmt.Sprintf("tmp%s", id)
}

func configurePauseContainerNetwork(config TRExConfig, pid int, br *netlink.Bridge, pauseID string) error {
	// 使用网络命名空间文件路径
	vethHost, vethCont := getPairName(pauseID)

	// 创建veth pair
	hostVeth, contVeth, err := createVethPair(vethHost, vethCont, 1500)
	if err != nil {
		return err
	}

	// 将host端veth连接到网桥
	if err := netlink.LinkSetMaster(hostVeth, br); err != nil {
		return fmt.Errorf("failed to connect veth to bridge: %v", err)
	}

	// 启用host端veth
	if err := netlink.LinkSetUp(hostVeth); err != nil {
		return fmt.Errorf("failed to set host veth up: %v", err)
	}
	netnsPath := fmt.Sprintf("/proc/%d/ns/net", pid)
	if err := netlink.LinkSetNsFd(contVeth, int(netnsPathFD(netnsPath))); err != nil {
		return fmt.Errorf("failed to move veth to container: %v", err)
	}

	// 配置VF vlanID
	// Todo ...

	// 进入网络命名空间配置
	return ns.WithNetNSPath(netnsPath, func(_ ns.NetNS) error {
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

func configVFNetwork(config TRExConfig) error {

	return nil
}
