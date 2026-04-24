package forge

// Set is a lookup of Forge implementations by Kind. It is the single
// collaborator callers (registry, poller, monitor, …) use to reach the
// correct adapter for a given RepoRef.
//
// A zero Set is usable; Register creates the internal map lazily.
type Set struct {
	forges map[Kind]Forge
}

// NewSet returns an empty Set.
func NewSet() *Set {
	return &Set{forges: map[Kind]Forge{}}
}

// Register installs f under its Kind. Re-registering the same kind replaces
// the previous entry.
func (s *Set) Register(f Forge) {
	if s.forges == nil {
		s.forges = map[Kind]Forge{}
	}
	s.forges[f.Kind()] = f
}

// For returns the adapter for ref. Returns *ErrUnknownForge if no adapter is
// registered for the ref's kind.
func (s *Set) For(ref RepoRef) (Forge, error) {
	f, ok := s.forges[ref.Forge]
	if !ok {
		return nil, &ErrUnknownForge{Kind: ref.Forge}
	}
	return f, nil
}

// Kinds returns the set of registered forge kinds. Order is not stable.
func (s *Set) Kinds() []Kind {
	out := make([]Kind, 0, len(s.forges))
	for k := range s.forges {
		out = append(out, k)
	}
	return out
}
