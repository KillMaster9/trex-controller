#!/bin/bash

# 构建 trexctl 客户端
echo "Building trexctl..."
cd client
go mod init trexctl
go get github.com/spf13/cobra
go build -o trexctl
mv trexctl ../bin/
cd ..

# 构建 trex-controller 服务端
echo "Building trex-controller..."
cd controller
docker build -t trex-controller .
cd ..

echo "Build complete. Executables in bin/, Docker image trex-controller built."