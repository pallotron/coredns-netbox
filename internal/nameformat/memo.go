package nameformat

import "sync"

// Memo caches Format results for one pass over the records (one poll). A
// device's name is needed for its own record, its aliases and each of its
// interface records; rendering it once means a broken template logs once
// per device per poll rather than once per record. Create a fresh Memo for
// every poll so template or device changes are picked up.
type Memo struct {
	f  *Formatter
	mu sync.Mutex
	m  map[string]memoEntry
}

type memoEntry struct {
	names Names
	ok    bool
}

// Memo returns an empty cache over f. It is safe on a nil Formatter (name
// formats off), whose Memo matches nothing, like Format.
func (f *Formatter) Memo() *Memo {
	return &Memo{f: f, m: make(map[string]memoEntry)}
}

// Format returns f.Format(deviceName), rendering each name at most once.
func (m *Memo) Format(deviceName string) (Names, bool) {
	if m == nil || m.f == nil {
		return Names{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.m[deviceName]; ok {
		return e.names, e.ok
	}
	names, ok := m.f.Format(deviceName)
	m.m[deviceName] = memoEntry{names: names, ok: ok}
	return names, ok
}
