package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

const (
	controllerURL = "http://localhost:21111" // trex-controller 地址
)

var rootCmd = &cobra.Command{
	Use:   "trexctl",
	Short: "TRex Controller CLI",
}

var applyCmd = &cobra.Command{
	Use:   "apply -f FILE",
	Short: "Apply configuration from file",
	Run:   applyHandler,
}

var updateCmd = &cobra.Command{
	Use:   "update -f FILE",
	Short: "Update configuration from file",
	Run:   updateHandler,
}

var deleteCmd = &cobra.Command{
	Use:   "delete -f FILE",
	Short: "Delete configuration from file",
	Run:   deleteHandler,
}

var file string

func init() {
	// 为所有命令添加文件标志
	applyCmd.Flags().StringVarP(&file, "file", "f", "", "Configuration file (required)")
	updateCmd.Flags().StringVarP(&file, "file", "f", "", "Configuration file (required)")
	deleteCmd.Flags().StringVarP(&file, "file", "f", "", "Configuration file (required)")

	// 标记文件标志为必需
	applyCmd.MarkFlagRequired("file")
	updateCmd.MarkFlagRequired("file")
	deleteCmd.MarkFlagRequired("file")

	// 添加子命令
	rootCmd.AddCommand(applyCmd, updateCmd, deleteCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// 发送请求到 trex-controller
func sendToController(action, filePath string) error {
	// 读取文件内容
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("error reading file: %w", err)
	}

	// 根据操作确定端点
	endpoint := ""
	switch action {
	case "apply":
		endpoint = "/apply"
	case "update":
		endpoint = "/update"
	case "delete":
		endpoint = "/delete"
	default:
		return fmt.Errorf("invalid action: %s", action)
	}

	// 创建请求
	url := controllerURL + endpoint
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(content))
	if err != nil {
		return fmt.Errorf("error creating request: %w", err)
	}

	// 设置内容类型（根据文件扩展名）
	ext := filepath.Ext(filePath)
	switch ext {
	case ".yaml", ".yml":
		req.Header.Set("Content-Type", "application/yaml")
	case ".json":
		req.Header.Set("Content-Type", "application/json")
	default:
		req.Header.Set("Content-Type", "text/plain")
	}

	// 发送请求
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}
	defer resp.Body.Close()

	// 处理响应
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", string(body))
	}

	// 解析成功响应
	//var result string
	//if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
	//	return fmt.Errorf("error decoding response: %w", err)
	//}
	//
	//fmt.Printf("Success: %s\n", result)
	return nil
}

// 命令处理函数
func applyHandler(cmd *cobra.Command, args []string) {
	if err := sendToController("apply", file); err != nil {
		fmt.Println("Apply failed:", err)
		os.Exit(1)
	}
}

func updateHandler(cmd *cobra.Command, args []string) {
	if err := sendToController("update", file); err != nil {
		fmt.Println("Update failed:", err)
		os.Exit(1)
	}
}

func deleteHandler(cmd *cobra.Command, args []string) {
	if err := sendToController("delete", file); err != nil {
		fmt.Println("Delete failed:", err)
		os.Exit(1)
	}
}
