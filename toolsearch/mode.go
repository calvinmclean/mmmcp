package toolsearch

// Mode controls which MCP tools are exposed and callable.
type Mode string

const (
	// ModeOff exposes only component tools.
	ModeOff Mode = "off"
	// ModeSearch exposes only the search and generic call tools.
	ModeSearch Mode = "search"
	// ModeHybrid exposes component and tool search tools together.
	ModeHybrid Mode = "hybrid"
)

// Valid reports whether the mode is supported. The zero value means off.
func (m Mode) Valid() bool {
	switch m {
	case "", ModeOff, ModeSearch, ModeHybrid:
		return true
	default:
		return false
	}
}

// Enabled reports whether tool search is available in this mode.
func (m Mode) Enabled() bool {
	return m == ModeSearch || m == ModeHybrid
}
