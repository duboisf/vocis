package config

import (
	"fmt"
	"strings"
)

// ExpandTemplate replaces {name} placeholders with values from vars.
// Missing placeholders are left as-is in the output.
func ExpandTemplate(template string, vars map[string]string) string {
	if template == "" || len(vars) == 0 {
		return template
	}
	replacements := make([]string, 0, len(vars)*2)
	for k, v := range vars {
		replacements = append(replacements, "{"+k+"}", v)
	}
	return strings.NewReplacer(replacements...).Replace(template)
}

// ValidateTemplate checks that all expected placeholders are present
// in the template. Returns a warning string for each missing placeholder.
func ValidateTemplate(template string, expected []string) []string {
	var warnings []string
	for _, name := range expected {
		if !strings.Contains(template, "{"+name+"}") {
			warnings = append(warnings, fmt.Sprintf("missing {%s} placeholder", name))
		}
	}
	return warnings
}
