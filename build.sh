#!/bin/bash

# 程序源文件名
SRC_FILE="dir2txt.go"
# 输出的基础名称
APP_NAME="dir2txt"
# 输出目录
BUILD_PATH="build"

echo "开始构建..."

# 确保输出目录存在
mkdir -p "$BUILD_PATH"

# 1. Linux amd64
echo "Building Linux (amd64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o "${BUILD_PATH}/${APP_NAME}_linux_amd64" $SRC_FILE

# 2. Linux arm64
echo "Building Linux (arm64)..."
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o "${BUILD_PATH}/${APP_NAME}_linux_arm64" $SRC_FILE

# 3. Windows amd64
echo "Building Windows (amd64)..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o "${BUILD_PATH}/${APP_NAME}_windows_amd64.exe" $SRC_FILE

# 4. Windows arm64
echo "Building Windows (arm64)..."
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -ldflags="-s -w" -o "${BUILD_PATH}/${APP_NAME}_windows_arm64.exe" $SRC_FILE

echo "构建完成！文件已生成在目录 ${BUILD_PATH}。"
ls -lh "$BUILD_PATH"