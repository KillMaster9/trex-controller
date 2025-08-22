package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/vishvananda/netlink"
	"os"
	"time"
)

// 检查进程是否存活
func isProcessAlive(pid int) bool {
	// 检查/proc/PID目录是否存在
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
		return false
	}
	return true
}

func createAndStartPauseContainer(ctx context.Context, config TRExConfig) (string, int, error) {
	name := config.Metadata.Name
	// 创建pause容器
	pauseName := fmt.Sprintf("%s-pause", name)
	resp, err := dockerClient.ContainerCreate(ctx, &container.Config{
		Image: pauseImage,
	}, &container.HostConfig{
		NetworkMode: "none",
	}, nil, nil, pauseName)

	if err != nil {
		return "", 0, fmt.Errorf("failed to create pause container: %v", err)
	}
	pauseID := resp.ID
	logger.Printf("Pause container %s created with ID: %s", pauseName, pauseID)

	// 启动pause容器
	if err := dockerClient.ContainerStart(ctx, pauseID, types.ContainerStartOptions{}); err != nil {
		return "", 0, fmt.Errorf("failed to start pause container: %v", err)
	}

	// 获取pause容器PID
	pid, err := getValidContainerPID(ctx, pauseID)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get pause container PID: %v", err)
	}

	return pauseID, pid, nil
}

func createWorkerContainer(ctx context.Context, config TRExConfig, pauseContainerID string, vfPCIMap map[string]string) (string, error) {
	image := config.Metadata.Image
	name := config.Metadata.Name
	// 生成配置文件
	configFilePath, err := createVFConfigFile(name, vfPCIMap)
	if err != nil {
		return "", fmt.Errorf("failed to create VF config file: %v", err)
	}

	// 创建工作容器配置
	containerConfig := &container.Config{
		Image: image,
		Cmd:   []string{"tail", "-f", "/dev/null"}, // 保持容器运行
		Tty:   true,
	}

	mounts := []mount.Mount{
		{
			Type:   mount.TypeBind,
			Source: "/mnt/huge",
			Target: "/mnt/huge",
		},
		{
			Type:   mount.TypeBind,
			Source: configFilePath,
			Target: "/etc/trex_cfg.yaml",
		},
	}

	hostConfig := &container.HostConfig{
		// 共享pause容器的网络命名空间
		NetworkMode: container.NetworkMode("container:" + pauseContainerID),
		// 添加所有能力
		CapAdd: strslice.StrSlice{"ALL"},
		// 启用特权模式
		Privileged: true,
		// 设置挂载点
		Mounts: mounts,
	}

	resp, err := dockerClient.ContainerCreate(ctx, containerConfig, hostConfig, nil, nil, config.Metadata.Name)
	if err != nil {
		return "", fmt.Errorf("failed to create worker container: %v", err)
	}
	workerID := resp.ID

	// 启动工作容器
	if err := dockerClient.ContainerStart(ctx, workerID, types.ContainerStartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start worker container: %v", err)
	}

	return workerID, nil
}

func cleanupOnError(ctx context.Context, state *deploymentState, config TRExConfig) {
	logger.Printf("Performing cleanup due to deployment failure")

	// 清理工作容器
	if state.workerContainerID != "" {
		logger.Printf("Removing worker container %s", state.workerContainerID)
		if err := dockerClient.ContainerRemove(ctx, state.workerContainerID, types.ContainerRemoveOptions{
			Force: true,
		}); err != nil {
			logger.Printf("Failed to remove worker container: %v", err)
		}
	}

	// 清理网络配置
	if state.networkConfigured {
		hostName, _ := getPairName(config.Metadata.Name, state.pauseContainerID)
		logger.Printf("Cleaning up network interfaces")
		if link, err := netlink.LinkByName(hostName); err == nil {
			netlink.LinkDel(link)
		}

		if config.Spec.NetworkType == "SRIOV" {
			// Todo...
		}
	}

	// 清理pause容器
	if state.pauseContainerID != "" {
		logger.Printf("Removing pause container %s", state.pauseContainerID)
		if err := dockerClient.ContainerRemove(ctx, state.pauseContainerID, types.ContainerRemoveOptions{
			Force: true,
		}); err != nil {
			logger.Printf("Failed to remove pause container: %v", err)
		}
	}
}

// 部署状态结构体
type deploymentState struct {
	bridgeCreated     bool
	pauseContainerID  string
	pausePID          int
	workerContainerID string
	networkConfigured bool
}

const pauseImage = "k8s.gcr.io/pause:3.8" // 官方轻量级pause容器

func CreateTRExContainer(ctx context.Context, config TRExConfig) (string, error) {
	state := &deploymentState{
		pauseContainerID:  "",
		workerContainerID: "",
	}
	bridgeName := config.Spec.BrName
	var err error

	defer func() {
		if err != nil {
			cleanupOnError(ctx, state, config)
		}
	}()

	// 1. 确保基础镜像存在
	if err = ensureImageExists(ctx, dockerClient, pauseImage); err != nil {
		return "", fmt.Errorf("failed to ensure pause image exists: %v", err)
	}
	if err = ensureImageExists(ctx, dockerClient, config.Metadata.Image); err != nil {
		return "", fmt.Errorf("failed to ensure TREx image exists: %v", err)
	}

	// 2. 确保网桥存在
	br, err := EnsureBridge(bridgeName, 1500, false, false)
	if err != nil {
		return "", fmt.Errorf("failed to ensure bridge: %v", err)
	}
	state.bridgeCreated = true

	// 3. 创建并启动pause容器
	pauseID, pid, err := createAndStartPauseContainer(ctx, config)
	if err != nil {
		return "", fmt.Errorf("failed to create pause container: %v", err)
	}
	state.pauseContainerID = pauseID
	state.pausePID = pid

	// 4. 配置pause容器的网络
	vfPCIMap, err := configurePauseContainerNetwork(config, pid, br, pauseID)
	if err != nil {
		return "", fmt.Errorf("failed to configure pause container network: %v", err)
	}
	state.networkConfigured = true

	// 5. 创建工作容器（共享pause容器的网络命名空间）
	workerID, err := createWorkerContainer(ctx, config, pauseID, vfPCIMap)
	if err != nil {
		return "", fmt.Errorf("failed to create worker container: %v", err)
	}
	state.workerContainerID = workerID

	return workerID, nil
}

func getValidContainerPID(ctx context.Context, containerID string) (int, error) {
	const maxRetries = 5
	const retryDelay = 500 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		containerJSON, err := dockerClient.ContainerInspect(ctx, containerID)
		if err != nil {
			return 0, fmt.Errorf("failed to inspect container: %v", err)
		}

		// 检查容器状态
		if containerJSON.State.Status != "running" {
			return 0, fmt.Errorf("container is not running, status: %s", containerJSON.State.Status)
		}

		pid := containerJSON.State.Pid
		if pid > 0 {
			// 验证PID是否有效
			if isProcessAlive(pid) {
				return pid, nil
			}
			logger.Printf("PID %d is not active, retrying...", pid)
		}

		time.Sleep(retryDelay)
	}

	return 0, fmt.Errorf("failed to get valid PID after %d retries", maxRetries)
}

func ensureImageExists(ctx context.Context, dockerClient *client.Client, image string) error {
	_, _, err := dockerClient.ImageInspectWithRaw(ctx, image)
	if err == nil {
		logger.Printf("Image already exists: %s", image)
		return nil
	}

	if !client.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect image %s: %v", image, err)
	}

	logger.Printf("Pulling image: %s", image)
	pullResp, err := dockerClient.ImagePull(ctx, image, types.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("failed to pull image %s: %v", image, err)
	}
	defer pullResp.Close()

	// 显示拉取进度
	scanner := bufio.NewScanner(pullResp)
	for scanner.Scan() {
		var status struct {
			Status string `json:"status"`
			ID     string `json:"id"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &status); err == nil {
			logger.Printf("Pulling image: %s - %s", status.ID, status.Status)
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Printf("Error reading pull response: %v", err)
	}

	logger.Printf("Successfully pulled image: %s", image)
	return nil
}

//func startAndValidateContainer(ctx context.Context, state *deploymentState) error {
//	logger.Printf("Starting container: %s", state.containerID)
//	if err := dockerClient.ContainerStart(ctx, state.containerID, types.ContainerStartOptions{}); err != nil {
//		return fmt.Errorf("failed to start container: %v", err)
//	}
//	state.containerStarted = true
//
//	// 等待容器进入运行状态
//	ctxTimeout, cancel := context.WithTimeout(ctx, 30*time.Second)
//	defer cancel()
//
//	statusCh, errCh := dockerClient.ContainerWait(ctxTimeout, state.containerID, container.WaitConditionNextExit)
//
//	select {
//	case status := <-statusCh:
//		if status.StatusCode != 0 {
//			logs, _ := dockerClient.ContainerLogs(ctx, state.containerID, types.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
//			defer logs.Close()
//			logContent, _ := ioutil.ReadAll(logs)
//			return fmt.Errorf("container exited unexpectedly with code %d\nLogs:\n%s", status.StatusCode, logContent)
//		}
//	case err := <-errCh:
//		// 忽略超时错误，表示容器仍在运行
//		if err != context.DeadlineExceeded {
//			return fmt.Errorf("error waiting for container: %v", err)
//		}
//	case <-ctxTimeout.Done():
//		// 超时表示容器仍在运行
//	}
//
//	return nil
//}
