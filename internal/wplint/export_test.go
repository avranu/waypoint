package wplint

// SetCoreOrder replaces corelayers' domain order for a test and returns what
// puts it back.
func SetCoreOrder(order ...string) (restore func()) {
	old := coreOrder
	coreOrder = order
	return func() { coreOrder = old }
}
