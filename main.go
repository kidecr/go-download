package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"crypto/tls"

	"github.com/cheggaaa/pb/v3"
)

const (
	defaultThreads    = 4
	bufferSize        = 1024 * 256 // 256KB 缓冲区
	defaultFileName   = "default.download.file"
	progressBatchSize = 1024 * 1024 // 1MB 进度批量更新
)

type DownloadTask struct {
	FileHash    string         `json:"file_hash"`    // 文件唯一标识
	TotalSize   int64            `json:"total_size"`   // 文件总大小
	Chunks      []ChunkProgress `json:"chunks"`      // 分块进度
	OutputPath  string         `json:"output_path"`  // 输出路径
	LastUpdated time.Time      `json:"last_updated"` // 最后更新时间
}

type ChunkProgress struct {
	Start      int64 		`json:"start"`
	End        int64 		`json:"end"`
	Downloaded int64	 	`json:"downloaded"` // 已下载字节数
}

type FileInfo struct {
	Size         int64
	SupportRange bool
}

var (
	progressFile string
	resumeFlag   bool
)

var (
	threads    int
	urlStr     string
	outputPath string
)

func init() {
	flag.BoolVar(&resumeFlag, "c", false, "Continue interrupted download")
	flag.IntVar(&threads, "t", defaultThreads, "Number of threads to use for downloading")
	flag.StringVar(&outputPath, "o", "", "Output path for the downloaded file")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("Usage: go run main.go -t <threads> -o <output> <url>")
		os.Exit(1)
	}
	urlStr = flag.Arg(0)
}

// 生成文件唯一标识
func generateFileID(fileSize int64, url string) string {
	h := sha256.New()
	h.Write([]byte(fmt.Sprintf("%s|%d", url, fileSize)))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// 加载或创建下载任务
func setupTask(client *http.Client, filePath string) (*DownloadTask, error) {
	progressFile = filePath + ".progress"

	// 尝试恢复任务
	if resumeFlag {
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
            fmt.Println("Output file missing, starting fresh")
            resumeFlag = false // 强制新下载
        } else {
			fmt.Println("progress file is ", progressFile)
			if task, err := loadProgress(progressFile); err == nil {
				if verifyTask(client, task) {
					fmt.Println("Resuming existing download")
					return task, nil
				}
				fmt.Println("Progress file not found, starting fresh")
			}
		}
	}

	// 不启用续传或验证失败时删除旧文件
    if !resumeFlag {
        os.Remove(progressFile)
        os.Remove(filePath)
    }

	// 创建新任务
	fileInfo, err := getFileInfo(client, urlStr)
	if err != nil {
		return nil, err
	}

	task := &DownloadTask{
		FileHash:   generateFileID(fileInfo.Size, urlStr),
		TotalSize:  fileInfo.Size,
		OutputPath: filePath,
		Chunks:     splitChunks(fileInfo.Size, threads, fileInfo.SupportRange),
	}

	if err := saveProgress(task); err != nil {
		return nil, fmt.Errorf("failed to create progress file: %v", err)
	}
	return task, nil
}

// 分块逻辑（支持动态调整）
func splitChunks(totalSize int64, threads int, rangeSupport bool) []ChunkProgress {
	if !rangeSupport || threads == 1 {
		return []ChunkProgress{{Start: 0, End: totalSize - 1}}
	}

	chunks := make([]ChunkProgress, threads)
	chunkSize := totalSize / int64(threads)
	for i := 0; i < threads; i++ {
		start := int64(i) * chunkSize
		end := start + chunkSize - 1
		if i == threads-1 {
			end = totalSize - 1
		}
		chunks[i] = ChunkProgress{Start: start, End: end}
	}
	return chunks
}

// 获取文件信息并检测服务器是否支持分块下载
func getFileInfo(client *http.Client, url string) (*FileInfo, error) {
	// 先尝试 HEAD 请求
	headResp, err := client.Head(url)
	if err == nil && headResp.StatusCode == http.StatusOK {
		// 检查 Accept-Ranges 头
		if headResp.Header.Get("Accept-Ranges") == "bytes" {
			size, _ := strconv.Atoi(headResp.Header.Get("Content-Length"))
			return &FileInfo{Size: int64(size), SupportRange: true}, nil
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
			return &FileInfo{Size: int64(size), SupportRange: true}, nil
		}
	}

	// 获取完整大小
	if resp.StatusCode == http.StatusOK {
		size, _ := strconv.Atoi(resp.Header.Get("Content-Length"))
		return &FileInfo{Size: int64(size), SupportRange: false}, nil
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

func downloadWithResume() error {
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:   30 * time.Second,
	}

	// 初始化下载任务
	filePath, err := determineFilePath()
	if err != nil {
		return err
	}
	task, err := setupTask(client, filePath)
	if err != nil {
		return err
	}

	// 打开输出文件
	file, err := os.OpenFile(task.OutputPath, os.O_WRONLY | os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer file.Close()
	
	// 创建带取消的上下文
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		wg           sync.WaitGroup
		errCh        = make(chan error, len(task.Chunks))
		errorOccurred atomic.Bool
		progressCh   = make(chan int, 10)
	)

	// 进度条初始化
	bar := pb.Full.Start64(int64(task.TotalSize))
	bar.Set(pb.Bytes, true)
	defer bar.Finish()

	// 启动进度保存协程
	go autoSaveProgress(task, 5*time.Second, ctx.Done())

	// 启动进度条更新：
	var wgbar sync.WaitGroup // 用于令进度条等待到下载完成再退出
    wgbar.Add(1)
    go func() {
		defer wgbar.Done()
        for n := range progressCh {
            bar.Add(n)
        }
		bar.Finish()
		fmt.Println("Progress bar finished")
		// 删除进度文件
		if resumeFlag { // 续传模式下，下载完成后删除进度文件
			fmt.Println("Download finished, Deleting progress file")
            os.Remove(progressFile)
        }
    }()

	// 启动下载协程
	for i, chunk := range task.Chunks {
		if chunk.Downloaded >= (chunk.End - chunk.Start + 1) {
			progressCh <- int(chunk.Downloaded)
			continue // 跳过已完成的分块
		}

		wg.Add(1)
		go func(chunkIdx int, cp ChunkProgress) {
			defer wg.Done()
			
			currentOffset := cp.Start + cp.Downloaded
			end := cp.End
			retries := 0

			for currentOffset <= end && !errorOccurred.Load() {
				req, _ := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", currentOffset, end))

				resp, err := client.Do(req)
				if err != nil {
					if retries < 3 {
						retries++
						time.Sleep(time.Duration(retries) * time.Second)
						continue
					}
					sendError(errCh, &errorOccurred, err)
					return
				}

				if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
					resp.Body.Close()
					sendError(errCh, &errorOccurred, fmt.Errorf("invalid status: %d", resp.StatusCode))
					return
				}

				// 下载分块
				buf := make([]byte, bufferSize)
				for !errorOccurred.Load() {
					n, err := resp.Body.Read(buf)
					if n > 0 {
						if _, err := file.WriteAt(buf[:n], int64(currentOffset)); err != nil {
							sendError(errCh, &errorOccurred, err)
							resp.Body.Close()
							return
						}
						// 更新进度
						progress := atomic.AddInt64(&task.Chunks[chunkIdx].Downloaded, int64(n))
						progressCh <- n
						currentOffset += int64(n)

						// 检查分块完成
						if int64(progress) >= (end - cp.Start + 1) {
							break
						}
					}
					if err != nil {
						if err == io.EOF {
							break
						}
						sendError(errCh, &errorOccurred, err)
						resp.Body.Close()
						return
					}
				}
				resp.Body.Close()
				retries = 0 // 重置重试计数
			}
		}(i, chunk)
	}

	// 错误监控
	go func() {
		wg.Wait()	// 等待所有下载协程完成
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
	
	wgbar.Wait() // 等待进度条协程处理完所有数据
	fmt.Printf("\nDownload completed successfully to: %s\n", filePath)
	return nil
}

// 自动保存进度
func autoSaveProgress(task *DownloadTask, interval time.Duration, stopCh <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			task.LastUpdated = time.Now()
			if err := saveProgress(task); err != nil {
				fmt.Printf("\nFailed to save progress: %v", err)
			}
		case <-stopCh:
			saveProgress(task)
			return
		}
	}
}

// 保存进度到文件
func saveProgress(task *DownloadTask) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return os.WriteFile(progressFile, data, 0644)
}

// 加载进度文件
func loadProgress(path string) (*DownloadTask, error) {
	fmt.Printf("Loading progress from: %s\n", path)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("Failed to load progress: %v\n", err)
		return nil, err
	}
	fmt.Printf("Loaded progress from: %s\n", path)

	var task DownloadTask
	if err := json.Unmarshal(data, &task); err != nil {
		fmt.Printf("Failed to unmarshal progress: %v\n", err)
		return nil, err
	}
	fmt.Printf("Verified progress from: %s\n", path)
	return &task, nil
}

// 验证任务有效性
func verifyTask(client *http.Client, task *DownloadTask) bool {
	// 检查原文件存在
    if _, err := os.Stat(task.OutputPath); os.IsNotExist(err) {
		fmt.Printf("File not found: %s\n", task.OutputPath)
        return false
    }

	// 检查文件大小是否变化
	fileInfo, err := getFileInfo(client, urlStr)
	if err != nil || fileInfo.Size != task.TotalSize {
		fmt.Printf("File size changed: file %d total %d\n", fileInfo.Size, task.TotalSize)
		return false
	}

	// 检查文件指纹
	if generateFileID(task.TotalSize, urlStr) != task.FileHash {
		fmt.Printf("File hash changed: %s\n", task.FileHash)
		return false
	}

	// 检查输出文件是否存在
	if _, err := os.Stat(task.OutputPath); os.IsNotExist(err) {
		fmt.Printf("Output file not found: %s\n", task.OutputPath)
		return false
	}

	return true
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
	if err := downloadWithResume(); err != nil {
		fmt.Printf("\nError: %v\n", err)
		os.Exit(1)
	}
}