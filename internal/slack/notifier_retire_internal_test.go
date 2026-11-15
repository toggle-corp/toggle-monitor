package slack

import (
	"testing"
	"time"
)

// TestLockDelivery_waiterOnRetiredRecord_takesTheFreshOne: a delivery
// that fetched a monitor's record and waited on its lock while the
// monitor was removed must not run on the retired record. It takes the
// record that replaced it, the one every later delivery locks too.
func TestLockDelivery_waiterOnRetiredRecord_takesTheFreshOne(t *testing.T) {
	n := &Notifier{}
	held := n.lockDelivery("api")

	got := make(chan *monitorDelivery, 1)
	go func() {
		d := n.lockDelivery("api")
		got <- d
		d.Unlock()
	}()
	time.Sleep(50 * time.Millisecond)
	n.retireDelivery("api", held)

	var waiter *monitorDelivery
	select {
	case waiter = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("lockDelivery did not return")
	}
	if waiter == held {
		t.Fatal("waiter locked the retired record")
	}
	n.mu.Lock()
	current := n.monitors["api"]
	n.mu.Unlock()
	if waiter != current {
		t.Errorf("waiter's record is not the monitor's current one")
	}
}
