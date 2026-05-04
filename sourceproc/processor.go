package sourceproc

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"windows-m3u-stream-merger-proxy/config"
	"windows-m3u-stream-merger-proxy/logger"
	"windows-m3u-stream-merger-proxy/utils"
)

type M3UProcessor struct {
	sync.RWMutex
	streamCount           atomic.Int64
	file                  *os.File
	writer                *bufio.Writer
	revalidatingDone      chan struct{}
	sortingMgr            *SortingManager
	criticalErrorOccurred atomic.Bool
}

func NewProcessor() *M3UProcessor {
	processedPath := config.GetNewM3UPath() + ".tmp"
	file, err := createResultFile(processedPath)
	if err != nil {
		logger.Default.Errorf("Error creating result file: %v", err)
		return nil
	}

	processor := &M3UProcessor{
		file:             file,
		writer:           bufio.NewWriter(file),
		revalidatingDone: make(chan struct{}),
		sortingMgr:       newSortingManager(),
	}

	return processor
}

func (p *M3UProcessor) Start(r *http.Request) {
	processCount := 0
	errors := p.processStreams(r)
	for err := range errors {
		if err != nil {
			logger.Default.Errorf("Error while processing stream: %v", err)
		}
		processCount++
		batch := int(math.Pow(10, math.Floor(math.Log10(float64(processCount)))))
		if batch < 100 {
			batch = 100
		}
		if processCount%batch == 0 {
			logger.Default.Logf("Processed %d streams so far", processCount)
		}
	}
	logger.Default.Logf("Completed processing %d total streams", processCount)
}

func (p *M3UProcessor) Wait(ctx context.Context) error {
	select {
	case <-p.revalidatingDone:
	case <-ctx.Done():
		logger.Default.Errorf("Revalidation failed due to context cancellation, keeping old data.")
		os.Remove(p.file.Name())
		p.cleanFailedRemoteFiles()

		return ctx.Err()
	}
	logger.Default.Debug("Finished revalidation")

	if !p.criticalErrorOccurred.Load() {
		logger.Default.Debug("Error has not occurred")
		prodPath := strings.TrimSuffix(p.file.Name(), ".tmp")

		logger.Default.Debugf("Renaming %s to %s", p.file.Name(), prodPath)
		err := os.Rename(p.file.Name(), prodPath)
		if err != nil {
			logger.Default.Errorf("Error renaming file: %v", err)
		}
		p.applyNewRemoteFiles()
		p.clearOldResults()
	} else {
		logger.Default.Errorf("Revalidation failed, keeping old data.")
		os.Remove(p.file.Name())
		p.file = nil
		p.cleanFailedRemoteFiles()
	}

	return nil
}

func (p *M3UProcessor) Run(ctx context.Context, r *http.Request) error {
	p.Start(r)
	return p.Wait(ctx)
}

func (p *M3UProcessor) GetCount() int {
	return int(p.streamCount.Load())
}

func (p *M3UProcessor) clearOldResults() {
	prodPath := strings.TrimSuffix(p.file.Name(), ".tmp")
	err := config.ClearOldProcessedM3U(prodPath)
	if err != nil {
		logger.Default.Error(err.Error())
	}
}

func (p *M3UProcessor) GetResultPath() string {
	if p.file == nil {
		LockSources()
		defer UnlockSources()

		path, err := config.GetLatestProcessedM3UPath()
		if err != nil {
			return ""
		}
		return path
	}
	prodPath := strings.TrimSuffix(p.file.Name(), ".tmp")
	return prodPath
}

func (p *M3UProcessor) markCriticalError(err error) {
	logger.Default.Errorf("Critical error during source processing: %v", err)
	p.criticalErrorOccurred.Store(true)
}

func (p *M3UProcessor) processStreams(r *http.Request) chan error {
	revalidating := true
	select {
	case _, revalidating = <-p.revalidatingDone:
	default:
	}

	if !revalidating {
		p.revalidatingDone = make(chan struct{})
	}

	results := streamDownloadM3USources()
	baseURL := utils.DetermineBaseURL(r)

	// Increase channel buffer sizes
	errors := make(chan error, 1000)         // Increased error buffer
	streamCh := make(chan *StreamInfo, 1000) // Larger stream buffer

	go func() {
		defer close(errors)
		defer p.cleanup()

		var wgProducers sync.WaitGroup
		for result := range results {
			wgProducers.Add(1)
			go func(res *SourceDownloaderResult) {
				defer wgProducers.Done()
				p.handleDownloaded(res, streamCh)
			}(result)
		}

		// Close streamCh after all producers finish
		go func() {
			wgProducers.Wait()
			close(streamCh)
		}()

		// Worker pool to process streams concurrently
		numWorkers := runtime.NumCPU() * 2
		var wgWorkers sync.WaitGroup
		wgWorkers.Add(numWorkers)

		for i := 0; i < numWorkers; i++ {
			go func() {
				defer wgWorkers.Done()
				for stream := range streamCh {
					err := p.addStream(stream)
					if err != nil {
						p.markCriticalError(err)
					}

					select {
					case errors <- err:
					default:
						logger.Default.Errorf("Error channel full, dropping error: %v", err)
					}
				}
			}()
		}

		wgWorkers.Wait() // Wait for all streams to be processed

		p.compileM3U(baseURL)
	}()

	return errors
}

func (p *M3UProcessor) applyNewRemoteFiles() {
	LockSources()
	defer UnlockSources()

	indexes := utils.GetM3UIndexes()

	for _, idx := range indexes {
		finalPath := utils.GetM3UFilePathByIndex(idx)
		tmpPath := finalPath + ".new"
		if _, err := os.Stat(tmpPath); err == nil {
			// Rename the temporary file to the final file.
			if err := os.Rename(tmpPath, finalPath); err != nil {
				logger.Default.Errorf("Error renaming remote file %s: %v", tmpPath, err)
			}
		}
	}

	currentSlugDir := config.GetCurrentSlugDirPath()
	newSlugDir := config.GetNewSlugDirPath()

	if err := os.MkdirAll(currentSlugDir, 0755); err != nil {
		logger.Default.Errorf("Error ensuring slug directory %s: %v", currentSlugDir, err)
		return
	}

	// Windows Server can deny directory-level remove/rename operations due to
	// transient handles. Publish slug updates via file-level sync instead.
	if err := clearDirFiles(currentSlugDir); err != nil {
		logger.Default.Errorf("Error clearing slug files in %s: %v", currentSlugDir, err)
	}
	if copyErr := copyDirContents(newSlugDir, currentSlugDir); copyErr != nil {
		logger.Default.Errorf("Error copying slug directory %s -> %s: %v", newSlugDir, currentSlugDir, copyErr)
	}
	_ = os.RemoveAll(newSlugDir)
}

func (p *M3UProcessor) cleanFailedRemoteFiles() {
	indexes := utils.GetM3UIndexes()
	for _, idx := range indexes {
		finalPath := utils.GetM3UFilePathByIndex(idx)
		tmpPath := finalPath + ".new"
		_ = os.RemoveAll(tmpPath)
	}
	_ = os.RemoveAll(config.GetNewSlugDirPath())
}

func (p *M3UProcessor) addStream(stream *StreamInfo) error {
	if stream == nil || stream.URLs.Size() == 0 {
		return nil
	}

	p.streamCount.Add(1)

	return p.sortingMgr.AddToSorter(stream)
}

func (p *M3UProcessor) compileM3U(baseURL string) {
	p.Lock()
	defer p.Unlock()

	defer func() {
		p.file.Close()
		p.sortingMgr.Close()
		close(p.revalidatingDone)
	}()

	_, err := p.writer.WriteString(utils.BuildPlaylistHeaderLine())
	if err != nil {
		p.markCriticalError(err)
		return
	}

	err = p.sortingMgr.GetSortedEntries(func(entry *StreamInfo) {
		_, writeErr := p.writer.WriteString(formatStreamEntry(baseURL, entry))
		if writeErr != nil {
			p.markCriticalError(writeErr)
		}
	})
	if err != nil {
		p.markCriticalError(err)
		return
	}

	if flushErr := p.writer.Flush(); flushErr != nil {
		p.markCriticalError(flushErr)
		return
	}
}

func (p *M3UProcessor) cleanup() {
	if p.writer != nil {
		p.writer.Flush()
	}
	if p.file != nil {
		p.file.Close()
	}
}

func (p *M3UProcessor) handleDownloaded(result *SourceDownloaderResult, streamCh chan<- *StreamInfo) {
	var currentLine string
	sourceConfig, hasSourceConfig := utils.GetSourceConfig(result.Index)
	treatAsSyntheticDiscovery := hasSourceConfig && isDiscoveredM3U8Source(result.Index, sourceConfig)
	parsedCount := 0

	// Handle errors asynchronously
	go func() {
		for err := range result.Error {
			if err != nil {
				logger.Default.Errorf("Error processing M3U %s: %v", result.Index, err)
			}
		}
	}()

	// Process lines as they come in
	for lineInfo := range result.Lines {
		line := strings.TrimSpace(lineInfo.Content)
		if treatAsSyntheticDiscovery && strings.HasPrefix(line, "#EXT-X-") {
			currentLine = ""
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") || strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			currentLine = line
		} else if currentLine != "" && !strings.HasPrefix(line, "#") {
			if streamInfo := parseLine(currentLine, lineInfo, result.Index); streamInfo != nil {
				parsedCount++
				if checkFilter(streamInfo) {
					streamCh <- streamInfo
				}
			}
			currentLine = ""
		}
	}

	if treatAsSyntheticDiscovery && parsedCount == 0 {
		if streamInfo := buildSyntheticDiscoveredStream(sourceConfig); streamInfo != nil && checkFilter(streamInfo) {
			streamCh <- streamInfo
		}
	}
}

func createResultFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
}

func copyDirContents(srcDir, dstDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		srcPath := filepath.Join(srcDir, entry.Name())
		dstPath := filepath.Join(dstDir, entry.Name())

		content, readErr := os.ReadFile(srcPath)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", srcPath, readErr)
		}
		if writeErr := os.WriteFile(dstPath, content, 0644); writeErr != nil {
			return fmt.Errorf("write %s: %w", dstPath, writeErr)
		}
	}

	return nil
}

func clearDirFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			// Slug mapping directories are expected to contain files only.
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		if removeErr := os.Remove(filePath); removeErr != nil {
			return fmt.Errorf("remove %s: %w", filePath, removeErr)
		}
	}
	return nil
}
