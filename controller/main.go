package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"gopkg.in/yaml.v2"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/natefinch/lumberjack"
	"github.com/vishvananda/netlink"
)

type Metadata struct {
	Name  string `json:"name" yaml:"name"`
	Image string `json:"image" yaml:"image"`
}
type Port struct {
	IFName  string `json:"ifName" yaml:"ifName"`
	VFIndex int    `json:"vfIndex" yaml:"vfIndex"`
	IP      string `json:"ip" yaml:"ip"`
	Gateway string `json:"gateway" yaml:"gateway"`
	VlanId  int    `json:"vlanId" yaml:"vlanId"`
}

type Spec struct {
	BrName          string `json:"brName" yaml:"brName"`
	MgmtIP          string `json:"mgmtIP" yaml:"mgmtIP"`
	MgmtGateway     string `json:"mgmtGateway" yaml:"mgmtGateway"`
	NetworkType     string `json:"networkType" yaml:"networkType"`
	ParentInterface string `json:"parantInterface" yaml:"parantInterface"`
	Port            []Port `json:"port" yaml:"port"`
}

// TRExConfig 定义TREx容器的配置
type TRExConfig struct {
	Kind     string   `json:"kind" yaml:"kind"` // 资源类型 TrexConfig
	Metadata Metadata `json:"metadata" yaml:"metadata"`
	Spec     Spec     `json:"spec" yaml:"spec"`
}

var (
	dockerClient *client.Client
	mu           sync.Mutex // 用于同步网络操作
	server       *http.Server
	logger       *log.Logger
	logFile      *os.File
)

// 命令行参数
var (
	logPath    = flag.String("log", "/var/log/trex-controller.log", "Path to log file")
	logLevel   = flag.String("level", "info", "Log level (debug, info, warn, error)")
	serverPort = flag.String("port", "21111", "Port to listen on")
)

func init() {
	// 解析命令行参数
	flag.Parse()

	// 创建日志目录（如果需要）
	logDir := filepath.Dir(*logPath)
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			log.Fatalf("Failed to create log directory: %v", err)
		}
	}

	// 设置日志轮转
	logRotator := &lumberjack.Logger{
		Filename: *logPath,
		Compress: true, // compress rotated logs
	}

	// 创建多目标日志写入器（文件和控制台）
	multiWriter := io.MultiWriter(os.Stdout, logRotator)

	// 创建自定义日志记录器
	logger = log.New(multiWriter, "", log.LstdFlags|log.Lmicroseconds|log.Lshortfile)

	// 初始化 Docker 客户端
	var err error
	dockerClient, err = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		logger.Fatalf("Error creating Docker client: %v", err)
	}

	logger.Printf("Logging initialized. Level: %s, Path: %s", *logLevel, *logPath)
}

func main() {
	logger.Println("Starting TREx Controller...")

	// 设置HTTP路由
	mux := http.NewServeMux()
	mux.HandleFunc("/apply", applyHandler)
	mux.HandleFunc("/update", updateHandler)
	mux.HandleFunc("/delete", deleteHandler)
	mux.HandleFunc("/health", healthHandler)

	// 创建HTTP服务器
	server = &http.Server{
		Addr:    fmt.Sprintf(":%s", *serverPort),
		Handler: mux,
	}

	// 在goroutine中启动服务器
	go func() {
		logger.Println(fmt.Sprintf("Starting HTTP server on :%s", *serverPort))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("HTTP server failed: %v", err)
		}
	}()

	// 设置优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	logger.Println("Shutting down server...")

	// 设置关闭超时
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		logger.Fatalf("Server forced to shutdown: %v", err)
	}

	logger.Println("Server exiting")
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func applyHandler(w http.ResponseWriter, r *http.Request) {
	handleRequest(w, r, "apply")
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	handleRequest(w, r, "update")
}

func deleteHandler(w http.ResponseWriter, r *http.Request) {
	handleRequest(w, r, "delete")
}

func handleRequest(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 关闭请求体避免资源泄露
	defer r.Body.Close()

	var config TRExConfig
	contentType := r.Header.Get("Content-Type")

	// 根据内容类型选择解码器
	if strings.Contains(contentType, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			logger.Printf("Error decoding request: %v", err)
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
	}

	if strings.Contains(contentType, "application/yaml") {
		if err := yaml.NewDecoder(r.Body).Decode(&config); err != nil {
			logger.Printf("Error decoding request: %v", err)
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}
	}

	logger.Printf("Received %s request for container: %s", action, config.Metadata.Name)

	var result string
	var err error

	switch action {
	case "apply":
		result, err = createTRExContainer(config)
	case "update":
		result, err = updateTRExContainer(config)
	case "delete":
		result, err = deleteTRExContainer(config)
	default:
		err = fmt.Errorf("unknown action: %s", action)
	}

	if err != nil {
		logger.Printf("%s failed for %s: %v", action, config.Metadata.Name, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(result))
	logger.Printf("%s completed for %s: %s", action, config.Metadata.Name, result)
}

// 生成trex开头的veth-pair网卡名称对
func generateTrexVethPair() (string, string) {
	// 初始化随机数生成器
	rand.Seed(time.Now().UnixNano())

	// 定义可用字符集：小写字母和数字
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	const suffixLength = 11

	// 生成11位随机后缀
	b := make([]byte, suffixLength)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	randomSuffix := string(b)

	// 生成主机端和容器端的veth名称
	vethHost := fmt.Sprintf("trex%s-h", randomSuffix) // h表示host端
	vethCont := fmt.Sprintf("trex%s-c", randomSuffix) // c表示container端

	return vethHost, vethCont
}

func createTRExContainer(config TRExConfig) (string, error) {
	name := config.Metadata.Name
	ctx := context.Background()
	mu.Lock()
	defer mu.Unlock()
	err := LoadConfig(&config)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %v", err)
	}

	logger.Printf("Creating container: %s", name)
	containers, err := dockerClient.ContainerList(ctx, types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list containers: %v", err)
	}

	for _, c := range containers {
		for _, cname := range c.Names {
			if strings.Contains(cname, name) {
				return "", fmt.Errorf("container with name %s already exists", name)
			}
		}
	}
	workloadId, err := CreateTRExContainer(ctx, config)
	if err != nil {
		return "", fmt.Errorf("failed to create TREx container: %v", err)
	}

	return fmt.Sprintf("Container %s created and started with ID: %s", name, workloadId), nil
}

func updateTRExContainer(config TRExConfig) (string, error) {
	name := config.Metadata.Name
	logger.Printf("Updating container: %s", name)
	// 简化实现：删除旧容器，创建新容器
	if _, err := deleteTRExContainer(config); err != nil {
		return "", err
	}

	err := LoadConfig(&config)
	if err != nil {
		return "", fmt.Errorf("failed to load config: %v", err)
	}

	return createTRExContainer(config)
}

func deleteTRExContainer(config TRExConfig) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	name := config.Metadata.Name

	pauseName := fmt.Sprintf("/%s-pause", name)
	workName := fmt.Sprintf("/%s", name)
	ctx := context.Background()

	logger.Printf("Deleting container: %s", name)
	// 查找容器
	containers, err := dockerClient.ContainerList(ctx, types.ContainerListOptions{
		All: true,
	})
	if err != nil {
		return "", nil
	}

	var containerID string
	var pauseID string

	for _, c := range containers {
		for _, cname := range c.Names {
			if strings.Compare(cname, workName) == 0 {
				containerID = c.ID
			}
			if strings.Compare(cname, pauseName) == 0 {
				pauseID = c.ID
			}
		}
	}

	if containerID == "" {
		return fmt.Sprintf("Container %s not exist", name), nil
	}
	if pauseID == "" {
		return fmt.Sprintf("Container %s not exist", pauseName), nil
	}

	logger.Printf("Stopping container: %s (ID: %s)", name, containerID)
	// 停止容器
	if err := dockerClient.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
		logger.Printf("Warning: failed to stop container %s: %v", containerID, err)
	}

	logger.Printf("Removing container: %s (ID: %s)", name, containerID)
	// 删除容器
	if err := dockerClient.ContainerRemove(ctx, containerID, types.ContainerRemoveOptions{
		Force: true,
	}); err != nil {
		return "", fmt.Errorf("failed to remove container: %v", err)
	}

	//删除Pause容器
	logger.Printf("Stopping pause container: %s (ID: %s)", pauseName, pauseID)
	if err := dockerClient.ContainerRemove(ctx, pauseID, types.ContainerRemoveOptions{
		Force: true,
	}); err != nil {
		return "", fmt.Errorf("failed to remove container: %v", err)
	}

	vethHost, vethCont := getPairName(config.Metadata.Name, pauseID)
	logger.Printf("Deleting veth pair: %s <-> %s", vethHost, vethCont)
	// 删除veth pair
	if err := deleteVethPair(vethHost); err != nil {
		logger.Printf("Warning: failed to delete veth pair: %v", err)
	}

	return fmt.Sprintf("Container %s deleted", name), nil
}

func deleteVethPair(vethHost string) error {
	// 删除主机端veth
	hostVeth, err := netlink.LinkByName(vethHost)
	if err != nil {
		return fmt.Errorf("failed to find host veth: %v", err)
	}
	if err := netlink.LinkDel(hostVeth); err != nil {
		return fmt.Errorf("failed to delete host veth: %v", err)
	}
	return nil
}
