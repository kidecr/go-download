package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cheggaaa/pb/v3"
)

const (
	defaultThreads    = 4
	bufferSize        = 1024 * 256 // 256KB 缓冲区
	defaultFileName   = "default.download.file"
	progressBatchSize = 1024 * 1024 // 1MB 进度批量更新
)

var (
	threads    int
	urlStr     string
	outputPath string
)

func init() {
	flag.IntVar(&threads, "t", defaultThreads, "Number of threads to use for downloading")
	flag.StringVar(&outputPath, "o", "", "Output path for the downloaded file")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("Usage: go run main.go -t <threads> -o <output> <url>")
		os.Exit(1)
	}
	urlStr = flag.Arg(0)
}

type FileInfo struct {
	Size         int
	SupportRange bool
}

// 获取文件信息并检测服务器是否支持分块下载
func getFileInfo(client *http.Client, url string) (*FileInfo, error) {
	// 先尝试 HEAD 请求
	headResp, err := client.Head(url)
	if err == nil && headResp.StatusCode == http.StatusOK {
		// 检查 Accept-Ranges 头
		if headResp.Header.Get("Accept-Ranges") == "bytes" {
			size, _ := strconv.Atoi(headResp.Header.Get("Content-Length"))
			return &FileInfo{Size: size, SupportRange: true}, nil
		}
	}

	// 回退到 GET 请求检测
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to detect range support: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range")
		if strings.HasPrefix(cr, "bytes 0-0/") {
			size, _ := strconv.Atoi(cr[len("bytes 0-0/"):])
			return &FileInfo{Size: size, SupportRange: true}, nil
		}
	}

	// 获取完整大小
	if resp.StatusCode == http.StatusOK {
		size, _ := strconv.Atoi(resp.Header.Get("Content-Length"))
		return &FileInfo{Size: size, SupportRange: false}, nil
	}

	return nil, fmt.Errorf("unable to determine file info")
}

func determineFilePath() (string, error) {
	if outputPath != "" {
		if dir := filepath.Dir(outputPath); dir != "." {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return "", fmt.Errorf("directory %s does not exist", dir)
			}
		}
		return outputPath, nil
	}

	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %v", err)
	}

	filename := filepath.Base(parsedURL.Path)
	if filename == "" || filename == "." {
		return defaultFileName, nil
	}
	return filename, nil
}

func downloadFile() error {
	// 创建带超时的HTTP客户端
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 30 * time.Second,
	}

	// 获取文件信息
	fileInfo, err := getFileInfo(client, urlStr)
	if err != nil {
		return fmt.Errorf("file info error: %v", err)
	}

	// 自动调整线程数
	if !fileInfo.SupportRange {
		threads = 1
		fmt.Println("Server does not support range requests, using single thread")
	}

	// 创建输出文件
	fullPath, err := determineFilePath()
	if err != nil {
		return err
	}
	
	file, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("file creation failed: %v", err)
	}
	defer file.Close()

	// 预分配磁盘空间
	if err := file.Truncate(int64(fileInfo.Size)); err != nil {
		return fmt.Errorf("disk space allocation failed: %v", err)
	}

	// 创建带取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		wg          sync.WaitGroup
		errCh       = make(chan error, threads)
		errorOccurred atomic.Bool
		progressCh = make(chan int, threads*2)
	)

	// 进度条处理
	bar := pb.Full.Start64(int64(fileInfo.Size))
	bar.Set(pb.Bytes, true)
	defer bar.Finish()

	// 启动进度更新器
	go func() {
		for bytes := range progressCh {
			bar.Add(bytes)
		}
	}()

	// 计算分块
	chunkSize := fileInfo.Size / threads
	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(threadID int) {
			defer wg.Done()

			start := threadID * chunkSize
			end := start + chunkSize - 1
			if threadID == threads-1 {
				end = fileInfo.Size - 1
			}

			// 创建带上下文的请求
			req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
			if err != nil {
				sendError(errCh, &errorOccurred, fmt.Errorf("request creation failed: %v", err))
				return
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

			resp, err := client.Do(req)
			if err != nil {
				sendError(errCh, &errorOccurred, fmt.Errorf("request failed: %v", err))
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				sendError(errCh, &errorOccurred, fmt.Errorf("unexpected status code: %d", resp.StatusCode))
				return
			}

			// 下载循环
			buf := make([]byte, bufferSize)
			bytesAccumulated := 0
			currentOffset := int64(start)

			for !errorOccurred.Load() {
				n, err := resp.Body.Read(buf)
				if err != nil && err != io.EOF {
					sendError(errCh, &errorOccurred, fmt.Errorf("read error: %v", err))
					return
				}

				if n > 0 {
					if _, err := file.WriteAt(buf[:n], currentOffset); err != nil {
						sendError(errCh, &errorOccurred, fmt.Errorf("write error: %v", err))
						return
					}
					currentOffset += int64(n)
					bytesAccumulated += n

					// 批量提交进度更新
					if bytesAccumulated >= progressBatchSize {
						select {
						case progressCh <- bytesAccumulated:
							bytesAccumulated = 0
						default:
						}
					}
				}

				if err == io.EOF {
					break
				}
			}

			// 提交剩余进度
			if bytesAccumulated > 0 {
				progressCh <- bytesAccumulated
			}
		}(i)
	}

	// 错误监控
	go func() {
		wg.Wait()
		close(progressCh)
		close(errCh)
	}()

	// 处理错误
	for err := range errCh {
		if err != nil {
			cancel() // 取消所有请求
			return fmt.Errorf("download failed: %v", err)
		}
	}

	fmt.Printf("\nDownload completed successfully to: %s\n", fullPath)
	return nil
}

func sendError(ch chan<- error, flag *atomic.Bool, err error) {
	if flag.CompareAndSwap(false, true) { // 保证只发送第一个错误
		select {
		case ch <- err:
		default:
		}
	}
}

func main() {
	if err := downloadFile(); err != nil {
		fmt.Printf("\nError: %v\n", err)
		os.Exit(1)
	}
}