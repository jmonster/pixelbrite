package browser

// either returns a channel closed once a or b is.
func either(a, b <-chan struct{}) <-chan struct{} {
	c := make(chan struct{})
	go func() {
		select {
		case <-a:
		case <-b:
		}
		close(c)
	}()
	return c
}
