package store

// dropFirstTerminal adapts UpsertStepReport's (firstTerminal, err) result for
// tests that only assert the error.
func dropFirstTerminal(_ bool, err error) error { return err }
