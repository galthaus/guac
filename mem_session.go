package guac

import (
	"sync"
)

type ConnId struct {
	Uuid string `json:"uuid"`
	Num  int    `json:"num"`
	Info string `json:"info"`
}
type ConnIds []*ConnId

// Len of array
func (c ConnIds) Len() int {
	return len(c)
}

// Less returns true if i < j
func (c ConnIds) Less(i, j int) bool {
	return c[i].Uuid < c[j].Uuid
}

// Swap swaps the elements with indexes i and j.
func (c ConnIds) Swap(i, j int) {
	ii, jj := c[i], c[j]
	c[i], c[j] = jj, ii
}

// MemorySessionStore is a simple in-memory store of connected sessions that is used by
// the WebsocketServer to store active sessions.
type MemorySessionStore struct {
	sync.RWMutex
	ConnIds map[string]*ConnId
}

// NewMemorySessionStore creates a new store
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		ConnIds: map[string]*ConnId{},
	}
}

// Get returns a connection by uuid
func (s *MemorySessionStore) Get(id string) int {
	s.RLock()
	defer s.RUnlock()
	return s.ConnIds[id].Num
}

// Add inserts a new connection by uuid
func (s *MemorySessionStore) Add(id, info string) {
	s.Lock()
	defer s.Unlock()
	if _, ok := s.ConnIds[id]; !ok {
		s.ConnIds[id] = &ConnId{Uuid: id, Num: 1, Info: info}
		return
	}
	s.ConnIds[id].Num = s.ConnIds[id].Num + 1
	return
}

// Delete removes a connection by uuid
func (s *MemorySessionStore) Delete(id string) {
	s.Lock()
	defer s.Unlock()
	c, ok := s.ConnIds[id]
	if !ok {
		return
	}
	if c.Num == 1 {
		delete(s.ConnIds, id)
		return
	}
	s.ConnIds[id].Num--
	return
}
