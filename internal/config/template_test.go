package config

import "testing"

func TestExpandTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		template string
		vars     map[string]string
		want     string
	}{
		{
			name:     "single placeholder",
			template: "Ready to type into {window}",
			vars:     map[string]string{"window": "kitty"},
			want:     "Ready to type into kitty",
		},
		{
			name:     "multiple placeholders",
			template: "Reconnecting... (attempt {attempt}/{max})",
			vars:     map[string]string{"attempt": "2", "max": "3"},
			want:     "Reconnecting... (attempt 2/3)",
		},
		{
			name:     "missing placeholder left as-is",
			template: "Ready to type into {window}",
			vars:     map[string]string{},
			want:     "Ready to type into {window}",
		},
		{
			name:     "no placeholders",
			template: "Voice typing is armed",
			vars:     map[string]string{"window": "kitty"},
			want:     "Voice typing is armed",
		},
		{
			name:     "empty template",
			template: "",
			vars:     map[string]string{"window": "kitty"},
			want:     "",
		},
		{
			name:     "extra vars ignored",
			template: "Hello",
			vars:     map[string]string{"window": "kitty", "foo": "bar"},
			want:     "Hello",
		},
		{
			name:     "repeated placeholder",
			template: "{name} is {name}",
			vars:     map[string]string{"name": "vocis"},
			want:     "vocis is vocis",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExpandTemplate(tt.template, tt.vars)
			if got != tt.want {
				t.Errorf("ExpandTemplate(%q, %v) = %q, want %q", tt.template, tt.vars, got, tt.want)
			}
		})
	}
}

func TestValidateTemplate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		template   string
		expected   []string
		wantErrors int
	}{
		{
			name:       "all placeholders present",
			template:   "attempt {attempt}/{max}",
			expected:   []string{"attempt", "max"},
			wantErrors: 0,
		},
		{
			name:       "missing one placeholder",
			template:   "Ready to type",
			expected:   []string{"window"},
			wantErrors: 1,
		},
		{
			name:       "missing multiple",
			template:   "Hello",
			expected:   []string{"window", "shortcut"},
			wantErrors: 2,
		},
		{
			name:       "no expected placeholders",
			template:   "Voice typing is armed",
			expected:   nil,
			wantErrors: 0,
		},
		{
			name:       "empty template with expected",
			template:   "",
			expected:   []string{"window"},
			wantErrors: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warnings := ValidateTemplate(tt.template, tt.expected)
			if len(warnings) != tt.wantErrors {
				t.Errorf("ValidateTemplate(%q, %v) returned %d warnings, want %d: %v",
					tt.template, tt.expected, len(warnings), tt.wantErrors, warnings)
			}
		})
	}
}
