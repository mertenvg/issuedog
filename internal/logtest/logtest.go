// Package logtest captures logr's output for tests. logr writes its queue
// from one goroutine, which can add a new writer before it writes lines
// already queued, so a test that logs and returns can leave lines queued
// that a writer added next would hold. A capture first waits for that queue
// to be written, and waits again before it ends, so it holds only what is
// logged while it runs. Every test that reads logr captures through this
// package, and is not parallel, since a capture takes the whole process's
// output.
package logtest

import (
	"bytes"
	"io"
	"sync"
	"testing"

	"github.com/mertenvg/logr/v2"
)

// Capture starts capturing logr's output at the levels in filter, and
// returns stop, which waits for logr to write what is queued, ends the
// capture and returns it. The test's cleanup stops a capture the test
// didn't.
func Capture(t testing.TB, filter logr.Type) (stop func() string) {
	t.Helper()
	var logs bytes.Buffer
	end := CaptureTo(t, &logs, filter)
	return func() string {
		end()
		return logs.String()
	}
}

// CaptureTo works as Capture does, but writes the capture to w: for a test
// that reads it before it stops, after a logr.Wait, or that takes each line
// as it comes. Its stop waits for logr to write what is queued and ends the
// capture.
func CaptureTo(t testing.TB, w io.Writer, filter logr.Type) (stop func()) {
	t.Helper()
	logr.Wait()
	end := logr.AddWriter(w, logr.WithFilter(filter))
	var once sync.Once
	stop = func() {
		once.Do(func() {
			logr.Wait()
			end()
		})
	}
	t.Cleanup(stop)
	return stop
}
