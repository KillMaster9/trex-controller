package main

import (
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"path/filepath"
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

func createVFConfigFile(name string, vfPCIMap map[string]string) (string, error) {
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

	for _, pciAddr := range vfPCIMap {
		pcis := []string{pciAddr, "dummy"}
		trexPortConfig.Interfaces = append(trexPortConfig.Interfaces, pcis...)
		ip, gateway, _ := generateRandomIPWithGateway()
		trexPortConfig.PortInfo = append(trexPortConfig.PortInfo, struct {
			ip             string `yaml:"ip"`
			defaultGateway string `yaml:"default_gateway"`
		}{ip, gateway})
	}

	vfConfigs := TrexConfigFile{
		TrexPortConfig: []TrexPortConfig{trexPortConfig},
	}

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
func generateRandomIPWithGateway() (string, string, error) {
	// 设置随机种子
	rand.Seed(time.Now().UnixNano())

	// 生成一个随机的私有IP地址段 (192.168.x.x/16)
	// 这里使用192.168.0.0/16网段作为示例
	privateIP := net.IPv4(192, 168, byte(rand.Intn(255)), byte(rand.Intn(254)+1))

	// 确保IP地址不为网络地址(.0)或广播地址(.255)
	for privateIP[3] == 0 || privateIP[3] == 255 {
		privateIP = net.IPv4(192, 168, byte(rand.Intn(255)), byte(rand.Intn(254)+1))
	}

	// 计算网关地址，通常为该网段的第一个可用地址(x.x.x.1)
	gatewayIP := net.IPv4(privateIP[0], privateIP[1], privateIP[2], 1)

	return privateIP.String(), gatewayIP.String(), nil
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
