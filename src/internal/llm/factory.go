package llm

import (
	"fmt"
	"sync"
)

type Constructor func(Config) (ModelClient, error)

var providers = struct {
	sync.RWMutex
	m map[string]Constructor
}{m: make(map[string]Constructor)}

func RegisterProvider(name string, constructor Constructor) {
	if name == "" || constructor == nil {
		panic("llm: invalid provider registration")
	}

	providers.Lock()
	defer providers.Unlock()
	providers.m[name] = constructor
}

func NewClient(name string, cfg Config) (ModelClient, error) {
	providers.RLock()
	constructor, ok := providers.m[name]
	providers.RUnlock()

	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q", name)
	}
	return constructor(cfg)
}
