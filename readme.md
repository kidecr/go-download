# Go Multi-Threaded Downloader

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
![Go Version](https://img.shields.io/github/go-mod/go-version/yourname/repo)

简易多线程命令行下载工具，支持动态URL和进度显示

## ✨ 功能特性

- ​**多线程加速**​ - 支持自定义线程数（默认4线程）
- ​**实时进度**​ - 可视化下载进度条和速率统计
- ​**智能重定向**​ - 自动处理CDN动态签名URL
- ​**断点保护**​ - 使用`WriteAt`实现分块写入（*注：非完整断点续传*）
- ​**路径验证**​ - 自动检测输出目录有效性

## 🚀 快速开始

### 安装方式

```bash
# 方式一：直接安装
go install github.com/yourname/downloader@latest

# 方式二：源码构建
git clone https://github.com/yourname/downloader.git
cd downloader
go build -o dl
```
## 使用示例
```bash
# 基础下载（自动识别文件名）
./dl -t 8 https://example.com/large-file.zip

# 指定输出路径
./dl -o ~/downloads/ubuntu.iso https://releases.ubuntu.com/22.04.4/ubuntu-22.04.4-desktop-amd64.iso

# 极速模式（使用32线程）
./dl -t 32 "https://objects.githubusercontent.com/...签名URL..."
```
## 🛠️ 技术实现
### 核心架构
```
+-----------------+
|   HTTP Client   |
+--------+--------+
         | 分片请求
+--------v--------+    +------------------+
|  Worker Threads +---->  File.WriteAt()  |
+--------+--------+    +------------------+
         |
+--------v--------+
| Progress Reporter|
+------------------+
```
### 依赖组件
|组件|	用途|	版本|
|-|-|-|
|cheggaaa/pb|	进度条显示|	v3|
|net/http|	HTTP客户端库|	标准库|
|os|	文件系统操作|	标准库|

## ⚠️ 注意事项   
### 动态URL时效性​  
从浏览器复制的CDN签名链接通常5-10分钟失效，建议直接使用原始URL

### 大文件存储​
确保目标磁盘有足够空间，下载前自动检测目录可写性

### ​网络中断处理​  
当前版本未实现完整断点续传功能，中断后需重新下载

## 📜 开源协议
MIT License - 自由使用和修改，需保留版权声明