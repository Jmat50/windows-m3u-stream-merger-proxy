package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGetLatestProcessedM3UPathIgnoresTmpAndDirs(t *testing.T) {
	tempDir := t.TempDir()
	t.Cleanup(func() {
		SetConfig(&Config{
			DataPath: "/windows-m3u-stream-merger-proxy/data/",
			TempPath: "/tmp/windows-m3u-stream-merger-proxy/",
		})
	})

	SetConfig(&Config{
		DataPath: tempDir,
		TempPath: filepath.Join(tempDir, "temp"),
	})

	processedDir := GetProcessedDirPath()
	if err := os.MkdirAll(filepath.Join(processedDir, "nested"), 0o755); err != nil {
		t.Fatalf("failed to create nested dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processedDir, "20260101120000.m3u.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatalf("failed to write tmp file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processedDir, "20260101130000.m3u"), []byte("a"), 0o644); err != nil {
		t.Fatalf("failed to write first m3u file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processedDir, "20260101140000.m3u"), []byte("b"), 0o644); err != nil {
		t.Fatalf("failed to write second m3u file: %v", err)
	}

	got, err := GetLatestProcessedM3UPath()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	want := filepath.Join(processedDir, "20260101140000.m3u")
	if got != want {
		t.Fatalf("unexpected latest path: got %q, want %q", got, want)
	}
}

func TestGetLatestProcessedM3UPathReturnsErrorWhenNoValidFiles(t *testing.T) {
	tempDir := t.TempDir()
	t.Cleanup(func() {
		SetConfig(&Config{
			DataPath: "/windows-m3u-stream-merger-proxy/data/",
			TempPath: "/tmp/windows-m3u-stream-merger-proxy/",
		})
	})

	SetConfig(&Config{
		DataPath: tempDir,
		TempPath: filepath.Join(tempDir, "temp"),
	})

	processedDir := GetProcessedDirPath()
	if err := os.MkdirAll(processedDir, 0o755); err != nil {
		t.Fatalf("failed to create processed dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(processedDir, "20260101120000.m3u.tmp"), []byte("tmp"), 0o644); err != nil {
		t.Fatalf("failed to write tmp file: %v", err)
	}

	_, err := GetLatestProcessedM3UPath()
	if err == nil {
		t.Fatal("expected error for missing valid processed files, got nil")
	}
}

