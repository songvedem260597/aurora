package official

import "testing"

func TestToolChoiceRequiresCall(t *testing.T) {
	forcedFunction := &ToolChoice{Type: "function", Function: &ToolChoiceFunction{Name: "bash"}}
	tests := []struct {
		name   string
		choice *ToolChoice
		want   bool
	}{
		{name: "nil", choice: nil, want: false},
		{name: "auto", choice: &ToolChoice{Type: "auto"}, want: false},
		{name: "none", choice: &ToolChoice{Type: "none"}, want: false},
		{name: "required", choice: &ToolChoice{Type: "required"}, want: true},
		{name: "any alias", choice: &ToolChoice{Type: "any"}, want: true},
		{name: "forced function", choice: forcedFunction, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.choice.RequiresCall(); got != tt.want {
				t.Fatalf("RequiresCall() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolChoiceNoneIsForcedNoneWithoutRequiringCall(t *testing.T) {
	choice := &ToolChoice{Type: "none"}
	if !choice.IsForcedNone() {
		t.Fatal("tool_choice=none must be recognized as forced none")
	}
	if choice.RequiresCall() {
		t.Fatal("tool_choice=none must never require a tool call")
	}
}
