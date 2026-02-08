package handler

import (
	"fmt"

	"k8s.io/klog/v2"
)

// HandlerRegistry maps device type + kind to handlers
type HandlerRegistry struct {
	handlers map[DeviceType]map[string]DeviceHandler // type -> kind -> handler
}

// NewHandlerRegistry creates a new empty handler registry
func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{
		handlers: make(map[DeviceType]map[string]DeviceHandler),
	}
}

// Register adds a handler to the registry for all its supported kinds
func (r *HandlerRegistry) Register(h DeviceHandler) {
	typ := h.Type()
	if _, ok := r.handlers[typ]; !ok {
		r.handlers[typ] = make(map[string]DeviceHandler)
	}
	for _, kind := range h.Kinds() {
		r.handlers[typ][kind] = h
		klog.Infof("Registered handler for type=%s kind=%s", typ, kind)
	}
}

// Get returns the handler for the given type and kind, or nil if not found
func (r *HandlerRegistry) Get(typ DeviceType, kind string) DeviceHandler {
	if kinds, ok := r.handlers[typ]; ok {
		return kinds[kind]
	}
	return nil
}

// MustGet returns the handler for the given type and kind, or returns an error
func (r *HandlerRegistry) MustGet(typ DeviceType, kind string) (DeviceHandler, error) {
	h := r.Get(typ, kind)
	if h == nil {
		return nil, fmt.Errorf("no handler registered for type=%s kind=%s", typ, kind)
	}
	return h, nil
}

// ListRegistered returns a summary of all registered handlers
func (r *HandlerRegistry) ListRegistered() map[DeviceType][]string {
	result := make(map[DeviceType][]string)
	for typ, kinds := range r.handlers {
		for kind := range kinds {
			result[typ] = append(result[typ], kind)
		}
	}
	return result
}
