package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	DataPath string
	TempPath string
}

var globalConfig = &Config{
	DataPath: "/windows-m3u-stream-merger-proxy/data/",
	TempPath: "/tmp/windows-m3u-stream-merger-proxy/",
}

func InitFromEnv() {
	dataPath := strings.TrimSpace(os.Getenv("DATA_PATH"))
	tempPath := strings.TrimSpace(os.Getenv("TEMP_PATH"))

	if dataPath == "" {
		dataPath = globalConfig.DataPath
	}
	if tempPath == "" {
		tempPath = globalConfig.TempPath
	}

	SetConfig(&Config{
		DataPath: dataPath,
		TempPath: tempPath,
	})
}

func GetConfig() *Config {
	return globalConfig
}

func SetConfig(c *Config) {
	globalConfig = c
}

func GetProcessedDirPath() string {
	return filepath.Join(globalConfig.DataPath, "processed/")
}

func GetCurrentSlugDirPath() string {
	return filepath.Join(globalConfig.DataPath, "slugs/")
}

func GetNewSlugDirPath() string {
	return filepath.Join(globalConfig.DataPath, "new-slugs/")
}

func GetLockFile() string {
	return filepath.Join(globalConfig.DataPath, ".lock")
}

func GetLatestProcessedM3UPath() (string, error) {
	dir := GetProcessedDirPath()
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to read directory: %w", err)
	}

	var validFiles []os.DirEntry
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if strings.HasSuffix(file.Name(), ".tmp") {
			continue
		}
		validFiles = append(validFiles, file)
	}

	if len(validFiles) == 0 {
		return "", fmt.Errorf("no processed m3u files found in directory")
	}

	return filepath.Join(dir, validFiles[len(validFiles)-1].Name()), nil
}

func GetNewM3UPath() string {
	now := time.Now()

	filename := now.Format("20060102150405")
	return filepath.Join(GetProcessedDirPath(), filename+".m3u")
}

func ClearOldProcessedM3U(latestFilename string) error {
	dir := GetProcessedDirPath()
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("failed to read directory: %w", err)
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		filePath := filepath.Join(dir, file.Name())

		if filePath == latestFilename {
			continue
		}

		err := os.Remove(filePath)
		if err != nil {
			return fmt.Errorf("failed to delete file %s: %w", filePath, err)
		}
	}

	return nil
}

func GetStreamsDirPath() string {
	return filepath.Join(globalConfig.DataPath, "streams/")
}

func GetSourcesDirPath() string {
	return filepath.Join(globalConfig.TempPath, "sources/")
}

func GetSortDirPath() string {
	return filepath.Join(globalConfig.TempPath, "sorter/")
}

