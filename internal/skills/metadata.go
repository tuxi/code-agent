package skills

import "fmt"

// ParseMeta validates a SKILL.md document and returns its frontmatter metadata.
// Bundle generators and installers use it so they enforce exactly the same
// frontmatter contract as the runtime loader.
func ParseMeta(content []byte) (Meta, error) {
	skill, err := parseSkill(string(content))
	if err != nil {
		return Meta{}, err
	}
	if skill.Name == "" || skill.Description == "" {
		return Meta{}, fmt.Errorf("skill metadata is incomplete")
	}
	return skill.Meta, nil
}
