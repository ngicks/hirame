package service

// WaitingOn reports how many callers are currently waiting on a thumbnail
// render.
//
// It is exported to the tests only. The count is what decides when a detached
// render is cancelled, and a test that tried to infer it from timing would
// prove nothing about the case it is there to cover: one caller leaving while
// another is still waiting.
func (t *Thumbnail) WaitingOn() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	var total int
	for _, party := range t.parties {
		total += party.waiting
	}
	return total
}
