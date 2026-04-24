package forge

// Set is a lookup of Forge implementations by Kind. Callers resolve the
// correct adapter for a RepoRef via For.
type Set struct {
	forges map[Kind]Forge
}

// NewSet returns an empty Set.
func NewSet() *Set {
	return &Set{forges: map[Kind]Forge{}}
}

// Register installs f under its Kind, replacing any previous entry.
func (s *Set) Register(f Forge) {
	s.forges[f.Kind()] = f
}

// For returns the adapter for ref, or *UnknownForgeError if unregistered.
func (s *Set) For(ref RepoRef) (Forge, error) {
	f, ok := s.forges[ref.Forge]
	if !ok {
		return nil, &UnknownForgeError{Kind: ref.Forge}
	}
	return f, nil
}

// Kinds returns registered forge kinds in unspecified order.
func (s *Set) Kinds() []Kind {
	out := make([]Kind, 0, len(s.forges))
	for k := range s.forges {
		out = append(out, k)
	}
	return out
}
