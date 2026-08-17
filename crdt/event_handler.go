package crdt

import "reflect"

type eventListener func(interface{}, interface{})

type eventHandler struct {
	listeners []eventListener
}

func newEventHandler() *eventHandler {
	return &eventHandler{}
}

// Adds an event listener that is called when
func addEventHandlerListener(eventHandler *eventHandler, f eventListener) {
	eventHandler.listeners = append(eventHandler.listeners, f)
}

// Removes an event listener.
func removeEventHandlerListener(eventHandler *eventHandler, f eventListener) {
	if eventHandler == nil {
		return
	}
	for i := len(eventHandler.listeners) - 1; i >= 0; i-- {
		if reflect.ValueOf(eventHandler.listeners[i]).Pointer() == reflect.ValueOf(f).Pointer() {
			eventHandler.listeners = append(eventHandler.listeners[:i], eventHandler.listeners[i+1:]...)
		}
	}
}

// Removes all event listeners.
func removeAllEventHandlerListeners(eventHandler *eventHandler) {
	if eventHandler == nil {
		return
	}
	eventHandler.listeners = []eventListener{}
}

// Call all event listeners that were added via
func callEventHandlerListeners(eventHandler *eventHandler, arg0, arg1 interface{}) {
	// A type integrated during a remote update may not have its event handler
	// initialized (e.g. a parent created via doc.Get with a bare constructor);
	// treat a nil handler as "no listeners" rather than dereferencing it.
	if eventHandler == nil {
		return
	}
	for _, f := range eventHandler.listeners {
		f(arg0, arg1)
	}
}
