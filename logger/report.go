package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type errorEvent struct {
	Timestamp string
	Level     string
	Message   string
	Caller    string
	Stack     string
}

type errorReporter struct {
	mu        sync.Mutex
	events    []errorEvent
	maxEvents int
	reportPath string
}

var reporter *errorReporter

func init() {
	reporter = newErrorReporter()
}

func newErrorReporter() *errorReporter {
	reportPath := os.Getenv("ERROR_REPORT_PATH")
	if strings.TrimSpace(reportPath) == "" {
		reportPath = filepath.Join(os.TempDir(), "m3u-stream-merger-proxy-error-report.txt")
	}
	return &errorReporter{
		maxEvents: 50,
		reportPath: reportPath,
	}
}

func (r *errorReporter) record(level, message string) {
	caller := captureCaller()
	stack := string(debug.Stack())
	event := errorEvent{
		Timestamp: time.Now().Format(time.RFC3339Nano),
		Level:     level,
		Message:   message,
		Caller:    caller,
		Stack:     stack,
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.events) >= r.maxEvents {
		r.events = r.events[1:]
	}
	r.events = append(r.events, event)
	_ = r.writeText()
}

func (r *errorReporter) writeText() error {
	if err := os.MkdirAll(filepath.Dir(r.reportPath), 0755); err != nil {
		return err
	}

	f, err := os.Create(r.reportPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := fmt.Fprintf(
		f,
		"M3U Stream Merger Proxy Error Report\nGenerated at: %s\nEvent count: %d\n\n",
		time.Now().Format(time.RFC3339Nano),
		len(r.events),
	); err != nil {
		return err
	}

	for i, event := range r.events {
		if _, err := fmt.Fprintf(
			f,
			"[%d/%d] %s %s\nMessage: %s\nCaller: %s\nStack:\n%s\n%s\n",
			i+1,
			len(r.events),
			event.Level,
			event.Timestamp,
			event.Message,
			event.Caller,
			event.Stack,
			strings.Repeat("-", 80),
		); err != nil {
			return err
		}
	}

	return nil
}

func captureCaller() string {
	pcs := make([]uintptr, 16)
	n := runtime.Callers(4, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		if !strings.Contains(frame.Function, "logger.") {
			return fmt.Sprintf("%s:%d %s", filepath.Base(frame.File), frame.Line, frame.Function)
		}
		if !more {
			return fmt.Sprintf("%s:%d %s", filepath.Base(frame.File), frame.Line, frame.Function)
		}
	}
}

func SetErrorReportPath(path string) {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	reporter.reportPath = path
}

func ResetErrorReport() {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	reporter.events = nil
}
