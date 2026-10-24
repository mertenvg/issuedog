package github

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mertenvg/logr/v2"
)

// CaptureLogs starts capturing logr's output at the levels in filter, and
// returns stop, which waits for logr to write what is queued, ends the
// capture and returns it. logr writes its queue from one goroutine, so a
// test that logs and returns can leave lines queued that a capture started
// next would hold. CaptureLogs first waits for that queue to be written, so
// the capture holds only what is logged once it starts. Every test in this
// package that reads logr captures through it, and is not parallel, since
// the capture takes the whole process's output. The test's cleanup stops a
// capture the test didn't. It is exported for the package's external tests.
func CaptureLogs(t *testing.T, filter logr.Type) (stop func() string) {
	t.Helper()
	logr.Wait()
	var logs bytes.Buffer
	end := logr.AddWriter(&logs, logr.WithFilter(filter))
	var once sync.Once
	stop = func() string {
		once.Do(func() {
			logr.Wait()
			end()
		})
		return logs.String()
	}
	t.Cleanup(func() { stop() })
	return stop
}

// A capture holds only what is logged once it starts, at its levels: not
// lines another test queued, which logr hadn't yet written, nor lines at
// other levels. Its stop returns once logr has written every line logged
// before it.
func TestCaptureLogsOwnLines(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	// The lines logged while logr's goroutine is held stay queued, as the
	// lines of a test that has just returned can.
	held := holdLogr(20 * time.Millisecond)
	logr.Warn("capture-logs-queued holding")
	for i := range 200 {
		logr.Warnf("capture-logs-queued %d", i)
	}
	stop := CaptureLogs(t, logr.W)
	held()
	held = holdLogr(20 * time.Millisecond)
	defer held()
	logr.Warn("capture-logs-own first")
	logr.Info("capture-logs-own info")
	for i := range 200 {
		logr.Warnf("capture-logs-own queued %d", i)
	}
	got := stop()
	if strings.Contains(got, "capture-logs-queued") {
		t.Errorf("capture %q; want none of the lines queued before it started", got)
	}
	if strings.Count(got, "capture-logs-own first") != 1 || strings.Count(got, "capture-logs-own queued ") != 200 {
		t.Errorf("capture %q; want each of its own warnings once, those queued behind a held line too", got)
	}
	if strings.Contains(got, "capture-logs-own info") {
		t.Errorf("capture %q; want no info line", got)
	}
}

// A capture the test didn't stop ends with the test, holding what was
// logged until then.
func TestCaptureLogsStoppedByCleanup(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	var stop func() string
	t.Run("unstopped", func(t *testing.T) {
		stop = CaptureLogs(t, logr.W)
		logr.Warn("capture-logs-cleanup during")
	})
	logr.Warn("capture-logs-cleanup after")
	logr.Wait()
	got := stop()
	if !strings.Contains(got, "capture-logs-cleanup during") || strings.Contains(got, "capture-logs-cleanup after") {
		t.Errorf("capture %q; want the warning logged during the test, and not the one after it", got)
	}
}

// holdLogr adds a logr writer for warnings that holds logr's one goroutine
// on each line it writes until d has passed, so the lines logged meanwhile
// stay queued. stop removes it.
func holdLogr(d time.Duration) (stop func()) {
	until := time.Now().Add(d)
	return logr.AddWriter(writerFunc(func(p []byte) (int, error) {
		time.Sleep(time.Until(until))
		return len(p), nil
	}), logr.WithFilter(logr.W))
}

// writerFunc is an io.Writer that is a function.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
