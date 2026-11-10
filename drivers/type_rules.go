package drivers

// TypeFamilyRule describes one database type's compatibility family and its
// width rank within that family. A larger rank means a wider value range.
// Rules are keyed by the uppercased base database type, without any
// parenthesised size or precision declaration (for example "VARCHAR").
type TypeFamilyRule struct {
	Family string `json:"family"`
	Rank   int    `json:"rank"`
}

// TypeRuleSet is the serializable, driver-owned description of explicit type
// compatibility rules. Types absent from the map, and columns carrying a
// domain, must be reported as unknown instead of assumed compatible.
type TypeRuleSet struct {
	Driver   string                    `json:"driver"`
	Families map[string]TypeFamilyRule `json:"families"`
}

// TypeRuleProvider is implemented by drivers that can describe their type
// compatibility rules explicitly. It allows out-of-process binary drivers to
// expose the same rules as side-effect imported in-process drivers.
type TypeRuleProvider interface {
	TypeRules() (*TypeRuleSet, error)
}
