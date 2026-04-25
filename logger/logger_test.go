package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestErrorReportCreatesTextReport(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "errors.txt")
	SetErrorReportPath(path)
	ResetErrorReport()

	logger := &DefaultLogger{}
	logger.Errorf("Test error detail %d", 1)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read error report: %v", err)
	}

	contents := string(data)
	if !strings.Contains(contents, "Test error detail 1") {
		t.Fatalf("error report did not include message, got: %s", contents)
	}
	if !strings.Contains(contents, "Stack:") {
		t.Fatalf("error report did not include stack details")
	}
}

func TestErrorReportLimitsTo50Entries(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "errors.txt")
	SetErrorReportPath(path)
	ResetErrorReport()

	logger := &DefaultLogger{}
	for i := 0; i < 55; i++ {
		logger.Errorf("Test error event %d", i)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read error report: %v", err)
	}

	contents := string(data)
	count := strings.Count(contents, "Message:")
	if count != 50 {
		t.Fatalf("expected 50 events in report, got %d", count)
	}
	if strings.Contains(contents, "Test error event 0") {
		t.Fatal("old event 0 should have been trimmed from the report")
	}
	if !strings.Contains(contents, "Test error event 54") {
		t.Fatal("latest event 54 should still be present")
	}
}
