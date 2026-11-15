package slack

// DeliveryRecords reports how many monitors the notifier holds an
// uptime delivery record for.
func (n *Notifier) DeliveryRecords() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.monitors)
}
