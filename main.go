package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/cheggaaa/pb/v3"
)

const (
	defaultThreads    = 4
	bufferSize        = 1024 * 32  // 使用更小的缓冲区以增加更新频率
	defaultFileName   = "default.download.file"
)

var (
	threads    int
	urlStr     string
	outputPath string
)

func init() {
	flag.IntVar(&threads, "t", defaultThreads, "Number of threads to use for downloading")
	flag.StringVar(&outputPath, "o", "", "Output path for the downloaded file (default: current directory)")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Println("Usage: go run main.go -t <number_of_threads> -o <output_path> <url>")
		os.Exit(1)
	}
	urlStr = flag.Arg(0)
}

func determineFilePath() (string, error) {
	if outputPath != "" {
		dir := filepath.Dir(outputPath)
		if dir != "." && dir != "" {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return "", fmt.Errorf("parent directory '%s' does not exist", dir)
			}
		}
		return outputPath, nil
	}

	u, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %v", err)
	}
	fileName := filepath.Base(u.Path)
	if fileName == "" || fileName == "." || fileName == "/" {
		fileName = defaultFileName
	}
	return fileName, nil
}

func downloadFile() error {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: nil},
	}
	client := &http.Client{Transport: tr}

	fullPath, err := determineFilePath()
	if err != nil {
		return err
	}

	resp, err := client.Head(urlStr)
	if err != nil {
		return fmt.Errorf("failed to get file info: %v", err)
	}
	defer resp.Body.Close()

	contentLength, _ := strconv.Atoi(resp.Header.Get("Content-Length"))
	if contentLength == 0 {
		return fmt.Errorf("content length is zero or not provided")
	}

	out, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer out.Close()

	partSize := contentLength / threads
	var wg sync.WaitGroup

	bar := pb.Full.Start64(int64(contentLength))
	bar.Set(pb.Bytes, true)
	startTime := time.Now()
	totalBytes := make(chan int)  // 使用无缓冲通道
	
	// 增加进度条等待组
	var pbwg sync.WaitGroup
	pbwg.Add(1)

	// 进度更新处理
	go func() {
		defer pbwg.Done()
		for bytes := range totalBytes {
			bar.Add(bytes)
		}
		// 结束时强制刷新
		bar.Finish()
	}()

	for i := 0; i < threads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start := i * partSize
			end := start + partSize - 1
			if i == threads-1 {
				end = contentLength - 1
			}

			req, err := http.NewRequest("GET", urlStr, nil)
			if err != nil {
				fmt.Printf("Thread %d failed to create request: %v\n", i, err)
				return
			}
			rangeHeader := fmt.Sprintf("bytes=%d-%d", start, end)
			req.Header.Set("Range", rangeHeader)

			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("Thread %d failed to fetch data: %v\n", i, err)
				return
			}
			defer resp.Body.Close()

			buf := make([]byte, bufferSize)
			for {
				n, err := resp.Body.Read(buf)
				if err != nil && err != io.EOF {
					fmt.Printf("Thread %d failed to read response body: %v\n", i, err)
					return
				}
				if n == 0 {
					break
				}

				_, err = out.WriteAt(buf[:n], int64(start))
				if err != nil {
					fmt.Printf("Thread %d failed to write to file: %v\n", i, err)
					return
				}
				start += n
				totalBytes <- n  // 每次读取后立即发送
			}
		}(i)
	}

	wg.Wait()
	close(totalBytes)  // 确保在所有线程完成后关闭通道
	pbwg.Wait() // 等待进度条彻底更新完成

	duration := time.Since(startTime).Seconds()
	totalDownloaded := float64(bar.Current())
	rate := totalDownloaded / duration / (1024 * 1024)
	fmt.Printf("\nDownload completed successfully.\nTotal rate: %.2f MB/s\n", rate)
	return nil
}

func main() {
	err := downloadFile()
	if err != nil {
		fmt.Printf("Error during download: %v\n", err)
		os.Exit(1)
	}
}
