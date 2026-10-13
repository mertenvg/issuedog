package githubtest

// ForgetWrites clears the write log, so Writes, FailWrite and Replay count
// from here: for a setup that runs passes of its own first.
func (f *Fake) ForgetWrites() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = nil
}
