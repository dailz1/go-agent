package skills

import "strings"

// catalogHeader leads every assembled catalog.
const catalogHeader = "Return the full instructions of a skill by name. Available skills:"

// assembleCatalog renders the fixed header plus one <skill> entry per skill.
// Input must already be name-sorted. Angle brackets are not escaped — the
// catalog is prompt text, not a security boundary, and parse-time whitespace
// collapsing keeps descriptions single-line so they cannot forge entries.
// An empty set renders the bare header.
func assembleCatalog(skills []Skill) string {
	var b strings.Builder
	b.WriteString(catalogHeader)
	for _, s := range skills {
		b.WriteString("\n<skill><name>")
		b.WriteString(s.Name)
		b.WriteString("</name><description>")
		b.WriteString(s.Description)
		b.WriteString("</description></skill>")
	}
	return b.String()
}
