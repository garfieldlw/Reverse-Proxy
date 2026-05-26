package balancer

import (
	"testing"
)

func TestNew(t *testing.T) {
	strategies := []struct {
		name    string
		want    string
	}{
		{"round_robin", "*balancer.RoundRobin"},
		{"weighted_round_robin", "*balancer.WeightedRoundRobin"},
		{"least_connections", "*balancer.LeastConnections"},
		{"random", "*balancer.Random"},
	}

	for _, tc := range strategies {
		t.Run(tc.name, func(t *testing.T) {
			b, err := New(tc.name)
			if err != nil {
				t.Fatalf("New(%q) returned error: %v", tc.name, err)
			}
			got := typeName(b)
			if got != tc.want {
				t.Errorf("New(%q) = %T, want %s", tc.name, b, tc.want)
			}
		})
	}
}

func TestNewInvalid(t *testing.T) {
	_, err := New("unknown")
	if err == nil {
		t.Fatal("New(\"unknown\") should return an error")
	}
}

func typeName(v interface{}) string {
	switch v.(type) {
	case *RoundRobin:
		return "*balancer.RoundRobin"
	case *WeightedRoundRobin:
		return "*balancer.WeightedRoundRobin"
	case *LeastConnections:
		return "*balancer.LeastConnections"
	case *Random:
		return "*balancer.Random"
	default:
		return "unknown"
	}
}
