package githubtest

import (
	"errors"
	"runtime"
)

// fakeFail fails the test New was given, for a helper called with
// something the fake hasn't got (fakeNumberIssue, fakeCommentID,
// DeleteRevision, DeleteIssue). A test's own call fails it with Fatalf. A
// call from a BeforeRequest or BeforeGraphQL hook runs off the test's
// goroutine, where Fatalf mustn't be called, so while a hook runs it fails
// the test with Errorf, saying it came from a hook, and ends the hook
// (fakeHookRun), whose request then gets a 500. A test that calls a helper
// wrongly itself while a hook runs is failed by Errorf too, and ended as
// testing reports a runtime.Goexit. f is locked.
func (f *Fake) fakeFail(format string, args ...any) {
	f.t.Helper()
	if f.hooksRunning == 0 {
		f.t.Fatalf(format, args...)
		return
	}
	f.t.Errorf(format+", in a BeforeRequest or BeforeGraphQL hook", args...)
	runtime.Goexit()
}

// errFakeHookFailed is the error of a hook that a helper's failure ended
// (fakeFail), which its request is answered with as a 500.
var errFakeHookFailed = errors.New("githubtest: a BeforeRequest or BeforeGraphQL hook failed the test")

// fakeHookRun runs do, a BeforeRequest or BeforeGraphQL hook, on a
// goroutine of its own, so that a helper's failure can end it (fakeFail)
// and the server still answers. It returns errFakeHookFailed if do ended
// before it returned, and passes on a panic in do, which the server
// recovers from as before. f is unlocked.
func (f *Fake) fakeHookRun(do func()) error {
	f.mu.Lock()
	f.hooksRunning++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.hooksRunning--
		f.mu.Unlock()
	}()
	var returned bool
	var panicked any
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { panicked = recover() }()
		do()
		returned = true
	}()
	<-done
	if panicked != nil {
		panic(panicked)
	}
	if !returned {
		return errFakeHookFailed
	}
	return nil
}
