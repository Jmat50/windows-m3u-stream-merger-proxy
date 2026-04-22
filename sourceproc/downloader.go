package sourceproc

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

type SourceDownloaderResult struct {
	Index string
	Lines chan *LineDetails
	Error chan error
}

type LineDetails struct {
	Content string
	LineNum int
}

func streamDownloadM3USources() chan *SourceDownloaderResult {
	resultChan := make(chan *SourceDownloaderResult)
	sources := utils.GetSourceConfigs()

	go func() {
		defer close(resultChan)
		var wg sync.WaitGroup

		for _, source := range sources {
			wg.Add(1)
			go func(source utils.SourceConfig) {
				defer wg.Done()

				result := &SourceDownloaderResult{
					Index: source.Index,
					Lines: make(chan *LineDetails, 1000),
					Error: make(chan error, 1),
				}

				go func() {
					defer close(result.Lines)
					defer close(result.Error)

					m3uURL := strings.TrimSpace(source.URL)
					if m3uURL == "" {
						result.Error <- fmt.Errorf("no URL configured for M3U index %s", source.Index)
						return
					}

					if strings.HasPrefix(m3uURL, "file://") {
						handleLocalFile(m3uURL, result)
						return
					}

					handleRemoteURL(m3uURL, source.Index, result)
				}()

				resultChan <- result
			}(source)
		}

		wg.Wait()
	}()

	return resultChan
}

func handleLocalFile(pathOrURL string, result *SourceDownloaderResult) {
	localPath := pathOrURL
	if strings.HasPrefix(pathOrURL, "file://") {
		resolvedPath, err := utils.FileURLToPath(pathOrURL)
		if err == nil {
			localPath = resolvedPath
		}
	}

	file, err := os.Open(localPath)
	if err != nil {
		result.Error <- fmt.Errorf("error opening local file: %v", err)
		return
	}
	defer file.Close()

	scanAndStream(file, result)
}

func handleRemoteURL(m3uURL, idx string, result *SourceDownloaderResult) {
	finalPath := utils.GetM3UFilePathByIndex(idx)
	tmpPath := finalPath + ".new"

	if err := os.MkdirAll(filepath.Dir(finalPath), os.ModePerm); err != nil {
		result.Error <- fmt.Errorf("error creating dir for source: %v", err)
		return
	}

	fallbackFile, fallbackErr := os.Open(finalPath)
	if fallbackErr != nil && !os.IsNotExist(fallbackErr) {
		logger.Default.Warnf("Unable to open existing fallback file for index %s: %v", idx, fallbackErr)
		fallbackFile = nil
	}
	defer func() {
		if fallbackFile != nil {
			fallbackFile.Close()
		}
	}()

	useFallback := func(err error) {
		if fallbackFile != nil {
			scanAndStream(fallbackFile, result)
		} else {
			result.Error <- err
		}
	}

	resp, err := utils.CustomHttpRequest(nil, "GET", m3uURL)
	if err != nil {
		logger.Default.Warnf("HTTP request error for index %s: %v", idx, err)
		useFallback(fmt.Errorf("HTTP request error: %v", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if fallbackFile != nil {
			logger.Default.Warnf("HTTP status %d for index %s. Falling back to existing file: %s", resp.StatusCode, idx, finalPath)
		} else {
			logger.Default.Warnf("HTTP status %d for index %s and no fallback exists", resp.StatusCode, idx)
		}
		useFallback(fmt.Errorf("HTTP status %d and no existing file", resp.StatusCode))
		return
	}

	bufReader := bufio.NewReader(resp.Body)
	isM3U, err := isM3UResponse(bufReader)
	if err != nil {
		if fallbackFile != nil {
			logger.Default.Warnf("Invalid M3U response for index %s. Falling back to existing file: %s", idx, finalPath)
		} else {
			logger.Default.Warnf("Invalid M3U response for index %s and no fallback exists: %v", idx, err)
		}
		useFallback(fmt.Errorf("invalid M3U response and no fallback: %v", err))
		return
	}
	if !isM3U {
		if fallbackFile != nil {
			logger.Default.Warnf("Invalid M3U response for index %s. Falling back to existing file: %s", idx, finalPath)
		} else {
			logger.Default.Warnf("Invalid M3U response for index %s and no fallback exists", idx)
		}
		useFallback(fmt.Errorf("invalid M3U response and no fallback"))
		return
	}

	newFile, err := os.Create(tmpPath)
	if err != nil {
		logger.Default.Warnf("Error creating tmp file for index %s: %v", idx, err)
		useFallback(fmt.Errorf("error creating tmp file: %v", err))
		return
	}
	defer newFile.Close()

	if fallbackFile != nil {
		fallbackFile.Close()
		fallbackFile = nil
	}

	reader := io.TeeReader(bufReader, newFile)
	scanAndStream(reader, result)
}

func isM3UResponse(r *bufio.Reader) (bool, error) {
	peekBytes, err := r.Peek(1024)
	if err != nil && err != bufio.ErrBufferFull && err != io.EOF {
		return false, err
	}
	return utils.IsM3UContent(peekBytes), nil
}

func scanAndStream(r io.Reader, result *SourceDownloaderResult) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		result.Lines <- &LineDetails{
			Content: scanner.Text(),
			LineNum: lineNum,
		}
		lineNum++
	}

	if err := scanner.Err(); err != nil {
		result.Error <- fmt.Errorf("error reading content: %v", err)
	}
}
