// Package testhooks holds the hooks tests use to make production code fail where
// nothing else can, as a ledger save partway through a reconcile. Being
// internal, it is reachable only from this module; every hook is nil outside a
// test that sets it, and a test that sets one must restore it.
package testhooks

// LedgerSave, when set, is called before every ledger save; an error it returns
// is the save's error, and nothing is written.
var LedgerSave func() error

// FailAfter is a LedgerSave that lets n saves through and fails every one after
// with err.
func FailAfter(n int, err error) func() error {
	return func() error {
		if n > 0 {
			n--
			return nil
		}
		return err
	}
}
