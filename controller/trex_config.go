package main

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type TrexPortConfig struct {
	PortLimit  int      `yaml:"port_limit"`
	Version    int      `yaml:"version"`
	Interfaces []string `yaml:"interfaces"`
	PortInfo   []struct {
		ip             string `yaml:"ip"`
		defaultGateway string `yaml:"default_gateway"`
	} `yaml:"port_info"`
}

type TrexConfigFile struct {
	TrexPortConfig []TrexPortConfig
}

func createVFConfigFile(name string, vfPCIMap map[string]string, config TRExConfig) (string, error) {
	// 转换映射格式
	trexPortConfig := TrexPortConfig{
		PortLimit:  len(vfPCIMap) * 2,
		Version:    2,
		Interfaces: make([]string, len(vfPCIMap)*2),
		PortInfo: make([]struct {
			ip             string `yaml:"ip"`
			defaultGateway string `yaml:"default_gateway"`
		}, len(vfPCIMap)*2),
	}

	pName := config.Spec.ParentInterface
	for i, port := range config.Spec.Port {
		vfName := fmt.Sprintf("%sv%d", pName, port.VFIndex)
		if pci, ok := vfPCIMap[vfName]; ok {
			trexPortConfig.Interfaces = append(trexPortConfig.Interfaces, pci, "dummy")
		} else {
			return "", fmt.Errorf("failed to find VF PCI address for %s", vfName)
		}

		var ip string
		var gateway string

		if port.IP != "" && port.Gateway != "" {
			ip = port.IP
			gateway = port.Gateway
		} else {
			ip, gateway = generateRandomIPWithGateway(i)
		}

		trexPortConfig.PortInfo = append(trexPortConfig.PortInfo, struct {
			ip             string `yaml:"ip"`
			defaultGateway string `yaml:"default_gateway"`
		}{ip, gateway})

		// this for dummy port
		tmpIP := strings.Split(ip, "/")[0]
		excludeIP := []net.IP{net.ParseIP(tmpIP), net.ParseIP(gateway)}
		dummyIP, _ := generateRandomIP(ip, excludeIP)
		trexPortConfig.PortInfo = append(trexPortConfig.PortInfo, struct {
			ip             string `yaml:"ip"`
			defaultGateway string `yaml:"default_gateway"`
		}{dummyIP.String(), gateway})
	}

	//for vfName, pciAddr := range vfPCIMap {
	//	pcis := []string{pciAddr, "dummy"}
	//	trexPortConfig.Interfaces = append(trexPortConfig.Interfaces, pcis...)
	//	ip, gateway, _ := generateRandomIPWithGateway()
	//	trexPortConfig.PortInfo = append(trexPortConfig.PortInfo, struct {
	//		ip             string `yaml:"ip"`
	//		defaultGateway string `yaml:"default_gateway"`
	//	}{ip, gateway})
	//}

	vfConfigs := TrexConfigFile{
		TrexPortConfig: []TrexPortConfig{trexPortConfig},
	}

	logger.Println("Create trex_cfg.yaml for %s:%v", name, trexPortConfig)

	// 转换为YAML格式
	yamlData, err := yaml.Marshal(vfConfigs)
	if err != nil {
		return "", fmt.Errorf("failed to marshal VF config to YAML: %v", err)
	}

	// 创建临时文件
	tmpDir := "/tmp/trex"
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create temp directory: %v", err)
	}

	tmpFile := filepath.Join(tmpDir, fmt.Sprintf("%s_trex_cfg.yaml", name))
	if err := ioutil.WriteFile(tmpFile, yamlData, 0644); err != nil {
		return "", fmt.Errorf("failed to write config file: %v", err)
	}

	return tmpFile, nil
}

// generateRandomIPWithGateway 随机生成一个IP地址和对应的网关
func generateRandomIPWithGateway(i int) (string, string) {
	// 设置随机种子
	return fmt.Sprintf("192.168.%d.%d/24", i, 10+i), fmt.Sprintf("192.168.%d.1", i)
}

func generateRandomIP(cidr string, excludeIP []net.IP) (net.IP, error) {
	// 解析CIDR
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	// 将IP转换为4字节格式

	// 计算网络大小
	ones, bits := ipNet.Mask.Size()
	totalIPs := 1 << (bits - ones)
	if totalIPs <= 1 {
		return nil, fmt.Errorf("network too small to generate random IP")
	}

	// 初始化随机数生成器
	rand.New(rand.NewSource(time.Now().UnixNano()))

	// 生成随机IP
	for {
		// 随机生成一个主机地址
		randomHost := rand.Uint32() % uint32(totalIPs)
		ip := make(net.IP, 4)
		ip[0] = ipNet.IP[0] + byte(randomHost>>24)
		ip[1] = ipNet.IP[1] + byte(randomHost>>16)
		ip[2] = ipNet.IP[2] + byte(randomHost>>8)
		ip[3] = ipNet.IP[3] + byte(randomHost)

		// 跳过网络地址和广播地址
		if randomHost == 0 || randomHost == uint32(totalIPs-1) {
			continue
		}

		// 跳过排除的IP
		for _, eIP := range excludeIP {
			eIP = eIP.To4()
			if eIP == nil {
				return nil, fmt.Errorf("excludeIP is not a valid IPv4 address")
			}
			if ip.Equal(eIP) {
				continue
			}
		}

		return ip, nil
	}
}

const brName = "trex-br0"

func LoadConfig(trexConfig *TRExConfig) error {
	if trexConfig == nil {
		return fmt.Errorf("trexConfig is nil, please configure trexConfig")
	}

	if trexConfig.Metadata.Name == "" {
		return fmt.Errorf("trexConfig.Metadata.Name is empty, please configure trexConfig.Metadata.Name")
	}

	if trexConfig.Metadata.Image == "" {
		return fmt.Errorf("trexConfig.Metadata.Image is empty, please configure trexConfig.Metadata.Image")
	}

	if trexConfig.Spec.MgmtIP == "" {
		return fmt.Errorf("trexConfig.Spec.MgmtIP is empty, please configure trexConfig.Spec.MgmtIP")
	}

	if trexConfig.Spec.MgmtGateway == "" {
		return fmt.Errorf("trexConfig.Spec.MgmtGateway is empty, please configure trexConfig.Spec.MgmtGateway")
	}

	if len(trexConfig.Spec.Port) == 0 {
		return fmt.Errorf("trexConfig.Spec.Port is empty, please configure trexConfig.Spec.Port")
	}

	if trexConfig.Spec.NetworkType == "" {
		trexConfig.Spec.NetworkType = "SRIOV"
	}

	if trexConfig.Spec.BrName == "" {
		trexConfig.Spec.BrName = brName
	}

	return nil
}
