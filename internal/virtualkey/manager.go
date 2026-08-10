package virtualkey

// Manager is the seam for future key management (CRUD, persistent store).
// For now the Store is loaded from config and the manager wraps it with the
// operations the gateway needs.
type Manager struct {
	store *Store
}

// NewManager wraps a store.
func NewManager(store *Store) *Manager {
	return &Manager{store: store}
}

// Authenticate validates a key and returns it, or nil.
func (m *Manager) Authenticate(value string) (*Key, bool) {
	return m.store.Lookup(value)
}
