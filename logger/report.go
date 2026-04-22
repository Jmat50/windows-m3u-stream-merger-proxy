package logger

import (
	"fmt"
	"html/template"
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
	htmlPath  string
}

var reporter *errorReporter

func init() {
	reporter = newErrorReporter()
}

func newErrorReporter() *errorReporter {
	htmlPath := os.Getenv("ERROR_REPORT_PATH")
	if strings.TrimSpace(htmlPath) == "" {
		htmlPath = filepath.Join(os.TempDir(), "m3u-stream-merger-proxy-error-report.html")
	}
	return &errorReporter{
		maxEvents: 50,
		htmlPath:  htmlPath,
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
	_ = r.writeHTML()
}

func (r *errorReporter) writeHTML() error {
	data := struct {
		GeneratedAt string
		Events      []errorEvent
	}{
		GeneratedAt: time.Now().Format(time.RFC3339Nano),
		Events:      append([]errorEvent(nil), r.events...),
	}

	tpl, err := template.New("report").Parse(htmlTemplate)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(r.htmlPath), 0755); err != nil {
		return err
	}

	f, err := os.Create(r.htmlPath)
	if err != nil {
		return err
	}
	defer f.Close()

	return tpl.Execute(f, data)
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
	reporter.htmlPath = path
}

func ResetErrorReport() {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	reporter.events = nil
}

const htmlTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8" />
<title>M3U Stream Merger Proxy Error Report</title>
<style>
body { font-family: Arial, sans-serif; margin: 0; padding: 16px; background: #f5f7fb; color: #111; }
header { margin-bottom: 24px; }
header h1 { margin: 0; font-size: 1.8rem; }
header p { margin: 4px 0 0; color: #555; }
.event { background: #fff; border: 1px solid #dce1e8; border-radius: 8px; margin-bottom: 12px; overflow: hidden; }
.summary { display: flex; align-items: center; justify-content: space-between; padding: 16px; cursor: pointer; }
.summary:hover { background: #f0f4fb; }
.summary .left { display: grid; gap: 4px; }
.summary .level { display: inline-block; padding: 4px 8px; border-radius: 4px; font-weight: 700; letter-spacing: .03em; }
.level-ERROR { background: #ffe5e5; color: #a00000; }
.level-WARN { background: #fff4d6; color: #7b5900; }
.summary .caller { color: #555; font-size: 0.9rem; }
.details { display: none; padding: 0 16px 16px 16px; border-top: 1px solid #e1e5ec; background: #fafbfc; }
.details pre { white-space: pre-wrap; word-break: break-word; margin: 0; font-size: 0.9rem; line-height: 1.4; }
.toggle { background: none; border: none; color: #1f5cff; cursor: pointer; font-size: 0.95rem; }
</style>
</head>
<body>
<header>
<h1>M3U Stream Merger Proxy Error Report</h1>
<p>Last {{len .Events}} logged error events. Generated at {{.GeneratedAt}}.</p>
</header>
{{range $index, $event := .Events}}
<div class="event">
	<div class="summary" onclick="toggleDetails({{$index}})">
		<div class="left">
			<div><span class="level level-{{$event.Level}}">{{$event.Level}}</span> {{$event.Timestamp}}</div>
			<div>{{$event.Message}}</div>
			<div class="caller">{{$event.Caller}}</div>
		</div>
		<button class="toggle" type="button">Show details</button>
	</div>
	<div id="details-{{$index}}" class="details">
		<pre>{{html $event.Stack}}</pre>
	</div>
</div>
{{end}}
<script>
function toggleDetails(index) {
	const details = document.getElementById('details-' + index);
	const button = details.previousElementSibling.querySelector('.toggle');
	if (details.style.display === 'block') {
		details.style.display = 'none';
		button.textContent = 'Show details';
	} else {
		details.style.display = 'block';
		button.textContent = 'Hide details';
	}
}
</script>
</body>
</html>`
