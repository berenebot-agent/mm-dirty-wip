package store

import "sync"

// outboundFaultForTest is a test-only, process-global one-shot fault injector for
// the outbound outcome and pre-send writes. Production never sets it; it exists
// so a test can deterministically fail a specific write at the exact point the
// accepted-provider-send safety contract must hold (an outcome write that fails
// after the provider accepted the mail), without relying on timing, a busy
// database or a real crash.
//
// It is deliberately narrow: the hook is consulted at the entry of the named
// store methods on the write path, matching by a caller-supplied token, and
// clears itself once it fires so a retry of the same method succeeds. It is a
// var (not wired into the store) because it models an infrastructure fault
// (disk/SQLite) rather than store state, and must be exercisable from an
// external app_test package.
var (
	outboundFaultMu    sync.Mutex
	outboundFaultPoint string
	outboundFaultErr   error
	outboundFaultMsgID string
)

// SetOutboundWriteFaultForTest makes the next write to the named point fail with
// err, but only when it targets messageID (empty matches any message). point is
// one of "MarkSent", "MarkFailed", "MarkWorkflowSent", "MarkWorkflowFailed",
// "BeginMessageSend" or "BeginWorkflowSend". The fault fires once and clears
// itself, so a subsequent retry of the same operation sees no fault. Pass an
// empty point to clear any pending fault.
func SetOutboundWriteFaultForTest(point, messageID string, err error) {
	outboundFaultMu.Lock()
	defer outboundFaultMu.Unlock()
	outboundFaultPoint = point
	outboundFaultMsgID = messageID
	outboundFaultErr = err
}

// consumeOutboundWriteFault reports whether the write at point for messageID
// must fail, and clears the pending fault when it does.
func consumeOutboundWriteFault(point, messageID string) error {
	outboundFaultMu.Lock()
	defer outboundFaultMu.Unlock()
	if outboundFaultPoint == "" || outboundFaultPoint != point || outboundFaultErr == nil {
		return nil
	}
	if outboundFaultMsgID != "" && messageID != "" && outboundFaultMsgID != messageID {
		return nil
	}
	err := outboundFaultErr
	outboundFaultPoint = ""
	outboundFaultMsgID = ""
	outboundFaultErr = nil
	return err
}
